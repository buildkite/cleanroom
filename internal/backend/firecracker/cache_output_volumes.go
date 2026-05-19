package firecracker

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
	charmlog "github.com/charmbracelet/log"
)

const defaultCacheOutputVolumeMinimumBytes int64 = cacheoutput.DefaultVolumeMinimumBytes
const cacheOutputGuestMountRoot = cacheoutput.GuestMountRoot

var (
	cacheOutputVolumeMinimumBytes     = defaultCacheOutputVolumeMinimumBytes
	createEmptyCacheOutputExt4ImageFn = createEmptyCacheOutputExt4Image
)

type preparedCacheOutputVolume struct {
	Spec   backend.CacheOutputVolumeSpec
	Drive  drive
	Volume volumestore.WritableVolume
}

func resolveCacheOutputVolumeMinimumBytes(cfg backend.FirecrackerConfig) int64 {
	if cfg.MinimumCacheOutputVolumeBytes > 0 {
		return cfg.MinimumCacheOutputVolumeBytes
	}
	return cacheOutputVolumeMinimumBytes
}

func effectiveCacheOutputVolumeMinimumBytes(cfg backend.FirecrackerConfig, spec backend.CacheOutputVolumeSpec) int64 {
	minimumBytes := resolveCacheOutputVolumeMinimumBytes(cfg)
	if spec.MinimumBytes > minimumBytes {
		return spec.MinimumBytes
	}
	return minimumBytes
}

func prepareCacheOutputVolumes(ctx context.Context, logger *charmlog.Logger, cfg backend.FirecrackerConfig, sandboxID, runDir string, specs []backend.CacheOutputVolumeSpec) ([]preparedCacheOutputVolume, func(), error) {
	if len(specs) == 0 {
		return nil, func() {}, nil
	}
	if strings.TrimSpace(sandboxID) == "" {
		return nil, nil, errors.New("missing sandbox id for cache output volumes")
	}
	if strings.TrimSpace(runDir) == "" {
		return nil, nil, errors.New("missing run dir for cache output volumes")
	}

	prepared := make([]preparedCacheOutputVolume, 0, len(specs))
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
		if _, ok := seenVolumeIDs[strings.TrimSpace(spec.VolumeID)]; ok {
			cleanup()
			return nil, nil, fmt.Errorf("cache output volume %d: duplicate volume id %q", i, spec.VolumeID)
		}
		seenVolumeIDs[strings.TrimSpace(spec.VolumeID)] = struct{}{}

		sourceRef, volumeCfg, err := cacheOutputVolumeSource(ctx, cfg, runDir, spec)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("cache output volume %q: %w", spec.VolumeID, err)
		}
		runtimeVolumeID := cacheOutputRuntimeVolumeID(sandboxID, spec.VolumeID, i)
		attachmentPath := filepath.Join(runDir, fmt.Sprintf("cache-output-%02d.ext4", i))
		volume, volumeCleanup, err := prepareWritableVolumeWithLogger(ctx, logger, volumeCfg, runtimeVolumeID, attachmentPath, sourceRef, effectiveCacheOutputVolumeMinimumBytes(volumeCfg, spec))
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("cache output volume %q: %w", spec.VolumeID, err)
		}
		cleanups = append(cleanups, volumeCleanup)
		prepared = append(prepared, preparedCacheOutputVolume{
			Spec: spec,
			Drive: drive{
				DriveID:      cacheOutputDriveID(i),
				PathOnHost:   volume.AttachmentPath,
				IsRootDevice: false,
				IsReadOnly:   false,
			},
			Volume: volume,
		})
	}
	return prepared, cleanup, nil
}

func cacheOutputVolumeSource(ctx context.Context, cfg backend.FirecrackerConfig, runDir string, spec backend.CacheOutputVolumeSpec) (sourceRef string, volumeCfg backend.FirecrackerConfig, err error) {
	sourceRef = cacheoutput.SourceRef(spec)
	volumeCfg = cfg
	if driver := strings.TrimSpace(spec.StorageDriver); driver != "" {
		volumeCfg.Snapshots.Driver = driver
	}
	if sourceRef == "" {
		sourceRef = filepath.Join(runDir, "cache-output-empty-base.ext4")
		if err := createEmptyCacheOutputExt4ImageFn(ctx, sourceRef, effectiveCacheOutputVolumeMinimumBytes(volumeCfg, spec)); err != nil {
			return "", volumeCfg, fmt.Errorf("create empty cache output volume source: %w", err)
		}
		return sourceRef, volumeCfg, nil
	}

	volumeCfg, err = snapshotConfigForStorageRef(volumeCfg, sourceRef)
	if err != nil {
		return "", volumeCfg, err
	}
	if driver := strings.TrimSpace(spec.StorageDriver); driver != "" {
		effectiveDriver := strings.TrimSpace(volumeCfg.Snapshots.Driver)
		if effectiveDriver == "" {
			effectiveDriver = "file"
		}
		if !strings.EqualFold(driver, effectiveDriver) {
			return "", volumeCfg, fmt.Errorf("storage driver %q does not match source driver %q", driver, effectiveDriver)
		}
	}
	return sourceRef, volumeCfg, nil
}

func createEmptyCacheOutputExt4Image(ctx context.Context, path string, minimumBytes int64) error {
	return cacheoutput.CreateEmptyExt4Image(ctx, path, minimumBytes)
}

func prepareWritableVolumeWithLogger(ctx context.Context, logger *charmlog.Logger, cfg backend.FirecrackerConfig, volumeID, attachmentPath, sourceRef string, minimumBytes int64) (volumestore.WritableVolume, func(), error) {
	driverCfg, err := snapshotConfigForStorageRef(cfg, sourceRef)
	if err != nil {
		return volumestore.WritableVolume{}, nil, err
	}
	if strings.EqualFold(strings.TrimSpace(driverCfg.Snapshots.Driver), "zfs") {
		hostRuntime := hostRuntimeForConfigWithLogger(driverCfg, logger)
		zfsReq := zfsWritableVolumeRequest{
			VolumeID:     volumeID,
			MinimumBytes: minimumBytes,
		}
		if filepath.IsAbs(strings.TrimSpace(sourceRef)) {
			zfsReq.SourcePath = sourceRef
		} else {
			zfsReq.SnapshotRef = sourceRef
		}
		volume, err := hostRuntime.PrepareZFSWritableVolume(ctx, zfsReq)
		if err != nil {
			return volumestore.WritableVolume{}, nil, err
		}
		cleanupVolume := func() {
			if err := hostRuntime.DestroyZFSVolume(context.Background(), volume.Ref); err != nil {
				logPersistentVolumeCleanup(logger, volume.Ref, err)
			}
		}
		return volumestore.WritableVolume{
			Ref:            volume.Ref,
			AttachmentPath: volume.AttachmentPath,
		}, cleanupVolume, nil
	}
	driver, err := rootFSVolumeStoreDriverFn(driverCfg)
	if err != nil {
		return volumestore.WritableVolume{}, nil, err
	}
	return preparePersistentWritableVolumeAtPathWithLogger(ctx, logger, driver, volumeID, attachmentPath, sourceRef, minimumBytes)
}

func preparePersistentWritableVolumeAtPathWithLogger(ctx context.Context, logger *charmlog.Logger, driver volumestore.Driver, volumeID, attachmentPath, sourceRef string, minimumBytes int64) (volumestore.WritableVolume, func(), error) {
	sourceRef = strings.TrimSpace(sourceRef)
	if sourceRef == "" {
		return volumestore.WritableVolume{}, nil, errors.New("missing persistent volume source")
	}

	resizeVolume := func(volume volumestore.WritableVolume) (volumestore.WritableVolume, func(), error) {
		cleanupVolume := func() {
			if err := driver.DestroyVolume(context.Background(), volumestore.DestroyVolumeRequest{VolumeRef: volume.Ref}); err != nil {
				logPersistentVolumeCleanup(logger, volume.Ref, err)
			}
		}
		if err := volumestore.EnsureWritableVolumeMinimumSize(ctx, driver, volume, minimumBytes); err != nil {
			cleanupVolume()
			return volumestore.WritableVolume{}, nil, fmt.Errorf("resize persistent volume: %w", err)
		}
		return volume, cleanupVolume, nil
	}

	if filepath.IsAbs(sourceRef) {
		rootfsPath, err := filepath.Abs(sourceRef)
		if err != nil {
			return volumestore.WritableVolume{}, nil, err
		}
		if _, err := os.Stat(rootfsPath); err != nil {
			return volumestore.WritableVolume{}, nil, fmt.Errorf("volume source %s: %w", rootfsPath, err)
		}

		baseVolume, err := driver.EnsureBaseVolume(ctx, volumestore.EnsureBaseVolumeRequest{
			BaseID:       strings.TrimSuffix(filepath.Base(rootfsPath), filepath.Ext(rootfsPath)),
			SourcePath:   rootfsPath,
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

func cacheOutputVolumeDrives(volumes []preparedCacheOutputVolume) []drive {
	if len(volumes) == 0 {
		return nil
	}
	drives := make([]drive, 0, len(volumes))
	for _, volume := range volumes {
		drives = append(drives, volume.Drive)
	}
	return drives
}

func cacheOutputDriveID(index int) string {
	return cacheoutput.MountID(index)
}

func cacheOutputRuntimeVolumeID(sandboxID, specVolumeID string, index int) string {
	return cacheoutput.RuntimeVolumeID(sandboxID, specVolumeID, index)
}

func cacheOutputVolumeMounts(volumes []preparedCacheOutputVolume) []vsockexec.CacheOutputMount {
	return cacheoutput.Mounts(cacheOutputPreparedMounts(volumes))
}

func cacheOutputPreparedMounts(volumes []preparedCacheOutputVolume) []cacheoutput.PreparedMount {
	if len(volumes) == 0 {
		return nil
	}
	mounts := make([]cacheoutput.PreparedMount, 0, len(volumes))
	for i, volume := range volumes {
		mounts = append(mounts, cacheoutput.PreparedMount{
			Spec:       volume.Spec,
			DevicePath: cacheOutputDevicePath(i),
			MountPath:  cacheOutputGuestMountPath(volume.Drive.DriveID),
		})
	}
	return mounts
}

func cacheOutputFileCaptures(volumes []preparedCacheOutputVolume, captures []backend.CacheOutputFileCapture) ([]vsockexec.CacheOutputFileCapture, error) {
	return cacheoutput.FileCaptures(cacheOutputPreparedMounts(volumes), captures)
}

func cloneCacheOutputMounts(mounts []vsockexec.CacheOutputMount) []vsockexec.CacheOutputMount {
	return cacheoutput.CloneMounts(mounts)
}

func vsockInputProjection(projection *backend.InputProjection) *vsockexec.InputProjection {
	if projection == nil {
		return nil
	}
	return &vsockexec.InputProjection{
		SourceRoot:          strings.TrimSpace(projection.SourceRoot),
		TargetRoot:          strings.TrimSpace(projection.TargetRoot),
		Files:               append([]string(nil), projection.Files...),
		MountSourceReadOnly: projection.MountSourceReadOnly,
	}
}

func cacheOutputGuestMountPath(driveID string) string {
	return cacheoutput.GuestMountPath(cacheOutputGuestMountRoot, driveID)
}

func cacheOutputSpecHasSource(spec backend.CacheOutputVolumeSpec) bool {
	return cacheoutput.SpecHasSource(spec)
}

func cacheOutputDevicePath(index int) string {
	return cacheoutput.DevicePathAfterRoot(index)
}
