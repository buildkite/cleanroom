//go:build darwin

package darwinvz

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/backend/cacheoutput"
	"github.com/buildkite/cleanroom/internal/volumestore"
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

const defaultDarwinVZCacheOutputVolumeMinimumBytes int64 = cacheoutput.DefaultVolumeMinimumBytes
const darwinVZCacheOutputGuestMountRoot = cacheoutput.GuestMountRoot

var (
	darwinVZCacheOutputVolumeMinimumBytes          = defaultDarwinVZCacheOutputVolumeMinimumBytes
	createEmptyDarwinVZCacheOutputExt4ImageFn      = createEmptyDarwinVZCacheOutputExt4Image
	prepareDarwinVZCacheOutputWritableVolumeFn     = prepareDarwinVZCacheOutputWritableVolume
)

type preparedDarwinVZCacheOutputVolume struct {
	Spec       backend.CacheOutputVolumeSpec
	Volume     volumestore.WritableVolume
	DevicePath string
	MountPath  string
}

func resolveDarwinVZCacheOutputVolumeMinimumBytes(cfg backend.FirecrackerConfig) int64 {
	if cfg.MinimumCacheOutputVolumeBytes > 0 {
		return cfg.MinimumCacheOutputVolumeBytes
	}
	return darwinVZCacheOutputVolumeMinimumBytes
}

func prepareDarwinVZCacheOutputVolumes(ctx context.Context, cfg backend.FirecrackerConfig, sandboxID, runDir string, specs []backend.CacheOutputVolumeSpec) ([]preparedDarwinVZCacheOutputVolume, func(), error) {
	if len(specs) == 0 {
		return nil, func() {}, nil
	}
	if strings.TrimSpace(sandboxID) == "" {
		return nil, nil, errors.New("missing sandbox id for cache output volumes")
	}
	if strings.TrimSpace(runDir) == "" {
		return nil, nil, errors.New("missing run dir for cache output volumes")
	}

	prepared := make([]preparedDarwinVZCacheOutputVolume, 0, len(specs))
	cleanups := make([]func(), 0, len(specs))
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	seenVolumeIDs := make(map[string]struct{}, len(specs))
	for i, spec := range specs {
		if err := cacheoutput.ValidateVolumeSpec(spec); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("cache output volume %d: %w", i, err)
		}
		volumeID := strings.TrimSpace(spec.VolumeID)
		if _, ok := seenVolumeIDs[volumeID]; ok {
			cleanup()
			return nil, nil, fmt.Errorf("cache output volume %d: duplicate volume id %q", i, spec.VolumeID)
		}
		seenVolumeIDs[volumeID] = struct{}{}

		sourceRef, volumeCfg, err := darwinVZCacheOutputVolumeSource(ctx, cfg, runDir, spec)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("cache output volume %q: %w", spec.VolumeID, err)
		}
		runtimeVolumeID := darwinVZCacheOutputRuntimeVolumeID(sandboxID, spec.VolumeID, i)
		attachmentPath := filepath.Join(runDir, fmt.Sprintf("cache-output-%02d.ext4", i))
		volume, volumeCleanup, err := prepareDarwinVZCacheOutputWritableVolumeFn(ctx, volumeCfg, runtimeVolumeID, attachmentPath, sourceRef, resolveDarwinVZCacheOutputVolumeMinimumBytes(volumeCfg))
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("cache output volume %q: %w", spec.VolumeID, err)
		}
		cleanups = append(cleanups, volumeCleanup)
		prepared = append(prepared, preparedDarwinVZCacheOutputVolume{
			Spec:       spec,
			Volume:     volume,
			DevicePath: darwinVZCacheOutputDevicePath(i),
			MountPath:  darwinVZCacheOutputGuestMountPath(i),
		})
	}

	return prepared, cleanup, nil
}

func darwinVZCacheOutputVolumeSource(ctx context.Context, cfg backend.FirecrackerConfig, runDir string, spec backend.CacheOutputVolumeSpec) (sourceRef string, volumeCfg backend.FirecrackerConfig, err error) {
	sourceRef = cacheoutput.SourceRef(spec)
	volumeCfg = cfg
	if driver := strings.TrimSpace(spec.StorageDriver); driver != "" {
		volumeCfg.Snapshots.Driver = driver
	}
	if sourceRef == "" {
		sourceRef = filepath.Join(runDir, "cache-output-empty-base.ext4")
		if err := createEmptyDarwinVZCacheOutputExt4ImageFn(ctx, sourceRef, resolveDarwinVZCacheOutputVolumeMinimumBytes(volumeCfg)); err != nil {
			return "", volumeCfg, fmt.Errorf("create empty cache output volume source: %w", err)
		}
		return sourceRef, volumeCfg, nil
	}
	if driver := strings.TrimSpace(spec.StorageDriver); driver != "" {
		effectiveDriver := strings.TrimSpace(volumeCfg.Snapshots.Driver)
		if effectiveDriver == "" {
			effectiveDriver = "apfs"
		}
		if !strings.EqualFold(driver, effectiveDriver) {
			return "", volumeCfg, fmt.Errorf("storage driver %q does not match source driver %q", driver, effectiveDriver)
		}
	}
	return sourceRef, volumeCfg, nil
}

func createEmptyDarwinVZCacheOutputExt4Image(ctx context.Context, path string, minimumBytes int64) error {
	return cacheoutput.CreateEmptyExt4Image(ctx, path, minimumBytes)
}

func prepareDarwinVZCacheOutputWritableVolume(ctx context.Context, cfg backend.FirecrackerConfig, volumeID, attachmentPath, sourceRef string, minimumBytes int64) (volumestore.WritableVolume, func(), error) {
	sourceRef = strings.TrimSpace(sourceRef)
	if sourceRef == "" {
		return volumestore.WritableVolume{}, nil, errors.New("missing persistent volume source")
	}
	driver, err := rootFSVolumeDriver(cfg)
	if err != nil {
		return volumestore.WritableVolume{}, nil, err
	}

	resizeVolume := func(volume volumestore.WritableVolume) (volumestore.WritableVolume, func(), error) {
		cleanup := func() {
			_ = driver.DestroyVolume(context.Background(), volumestore.DestroyVolumeRequest{VolumeRef: volume.Ref})
		}
		if err := volumestore.EnsureWritableVolumeMinimumSize(ctx, driver, volume, minimumBytes); err != nil {
			cleanup()
			return volumestore.WritableVolume{}, nil, fmt.Errorf("resize persistent volume: %w", err)
		}
		return volume, cleanup, nil
	}

	if filepath.IsAbs(sourceRef) {
		sourcePath, err := filepath.Abs(sourceRef)
		if err != nil {
			return volumestore.WritableVolume{}, nil, err
		}
		if _, err := os.Stat(sourcePath); err != nil {
			return volumestore.WritableVolume{}, nil, fmt.Errorf("volume source %s: %w", sourcePath, err)
		}
		baseVolume, err := driver.EnsureBaseVolume(ctx, volumestore.EnsureBaseVolumeRequest{
			BaseID:       strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath)),
			SourcePath:   sourcePath,
			MinimumBytes: minimumBytes,
		})
		if err != nil {
			return volumestore.WritableVolume{}, nil, fmt.Errorf("prepare base volume: %w", err)
		}
		volume, err := driver.CreateWritableVolume(ctx, volumestore.CreateWritableVolumeRequest{
			VolumeID:       volumeID,
			BaseRef:        baseVolume.Ref,
			AttachmentPath: attachmentPath,
		})
		if err != nil {
			return volumestore.WritableVolume{}, nil, err
		}
		return resizeVolume(volume)
	}

	volume, err := driver.CloneSnapshotToVolume(ctx, volumestore.CloneSnapshotToVolumeRequest{
		VolumeID:       volumeID,
		SnapshotRef:    sourceRef,
		AttachmentPath: attachmentPath,
	})
	if err != nil {
		return volumestore.WritableVolume{}, nil, err
	}
	return resizeVolume(volume)
}

func darwinVZCacheOutputDiskPaths(volumes []preparedDarwinVZCacheOutputVolume) []string {
	if len(volumes) == 0 {
		return nil
	}
	paths := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		paths = append(paths, volume.Volume.AttachmentPath)
	}
	return paths
}

func darwinVZCacheOutputVolumeMounts(volumes []preparedDarwinVZCacheOutputVolume) []vsockexec.CacheOutputMount {
	return cacheoutput.Mounts(darwinVZCacheOutputPreparedMounts(volumes))
}

func darwinVZCacheOutputPreparedMounts(volumes []preparedDarwinVZCacheOutputVolume) []cacheoutput.PreparedMount {
	if len(volumes) == 0 {
		return nil
	}
	mounts := make([]cacheoutput.PreparedMount, 0, len(volumes))
	for _, volume := range volumes {
		mounts = append(mounts, cacheoutput.PreparedMount{
			Spec:       volume.Spec,
			DevicePath: volume.DevicePath,
			MountPath:  volume.MountPath,
		})
	}
	return mounts
}

func darwinVZCacheOutputFileCaptures(volumes []preparedDarwinVZCacheOutputVolume, captures []backend.CacheOutputFileCapture) ([]vsockexec.CacheOutputFileCapture, error) {
	return cacheoutput.FileCaptures(darwinVZCacheOutputPreparedMounts(volumes), captures)
}

func cloneDarwinVZCacheOutputMounts(mounts []vsockexec.CacheOutputMount) []vsockexec.CacheOutputMount {
	return cacheoutput.CloneMounts(mounts)
}

func darwinVZCacheOutputSpecHasSource(spec backend.CacheOutputVolumeSpec) bool {
	return cacheoutput.SpecHasSource(spec)
}

func darwinVZCacheOutputRuntimeVolumeID(sandboxID, specVolumeID string, index int) string {
	return cacheoutput.RuntimeVolumeID(sandboxID, specVolumeID, index)
}

func darwinVZCacheOutputGuestMountPath(index int) string {
	return cacheoutput.GuestMountPath(darwinVZCacheOutputGuestMountRoot, cacheoutput.MountID(index))
}

func darwinVZCacheOutputDevicePath(index int) string {
	return cacheoutput.DevicePathAfterRoot(index)
}
