//go:build darwin

package darwinvz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/ext4image"
	"github.com/buildkite/cleanroom/internal/hosttools"
	"github.com/buildkite/cleanroom/internal/volumestore"
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

const defaultDarwinVZCacheOutputVolumeMinimumBytes int64 = 4 << 30
const darwinVZCacheOutputGuestMountRoot = "/run/cleanroom/cache-output-volumes"

var (
	darwinVZCacheOutputVolumeMinimumBytes     = defaultDarwinVZCacheOutputVolumeMinimumBytes
	createEmptyDarwinVZCacheOutputExt4ImageFn = createEmptyDarwinVZCacheOutputExt4Image
)

type preparedDarwinVZCacheOutputVolume struct {
	Spec       backend.CacheOutputVolumeSpec
	Volume     volumestore.WritableVolume
	DevicePath string
	MountPath  string
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
		if err := validateDarwinVZCacheOutputVolumeSpec(spec); err != nil {
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
		volume, volumeCleanup, err := prepareDarwinVZCacheOutputWritableVolume(ctx, volumeCfg, runtimeVolumeID, attachmentPath, sourceRef, darwinVZCacheOutputVolumeMinimumBytes)
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

func validateDarwinVZCacheOutputVolumeSpec(spec backend.CacheOutputVolumeSpec) error {
	if strings.TrimSpace(spec.Stage) == "" {
		return errors.New("missing stage")
	}
	if strings.TrimSpace(spec.BlockName) == "" {
		return errors.New("missing block name")
	}
	if strings.TrimSpace(spec.CacheKey) == "" {
		return errors.New("missing cache key")
	}
	if strings.TrimSpace(spec.VolumeID) == "" {
		return errors.New("missing volume id")
	}
	if len(spec.DirMappings) == 0 && len(spec.FileMappings) == 0 {
		return errors.New("missing output mappings")
	}
	for i, mapping := range spec.DirMappings {
		if strings.TrimSpace(mapping.GuestPath) == "" {
			return fmt.Errorf("dir mapping %d missing guest path", i)
		}
		if strings.TrimSpace(mapping.Subpath) == "" {
			return fmt.Errorf("dir mapping %d missing volume subpath", i)
		}
	}
	for i, mapping := range spec.FileMappings {
		if strings.TrimSpace(mapping.GuestPath) == "" {
			return fmt.Errorf("file mapping %d missing guest path", i)
		}
		if strings.TrimSpace(mapping.Subpath) == "" {
			return fmt.Errorf("file mapping %d missing volume subpath", i)
		}
	}
	return nil
}

func darwinVZCacheOutputVolumeSource(ctx context.Context, cfg backend.FirecrackerConfig, runDir string, spec backend.CacheOutputVolumeSpec) (sourceRef string, volumeCfg backend.FirecrackerConfig, err error) {
	sourceRef = strings.TrimSpace(spec.SourceSnapshotRef)
	if sourceRef == "" {
		sourceRef = strings.TrimSpace(spec.StorageRef)
	}
	volumeCfg = cfg
	if driver := strings.TrimSpace(spec.StorageDriver); driver != "" {
		volumeCfg.Snapshots.Driver = driver
	}
	if sourceRef == "" {
		sourceRef = filepath.Join(runDir, "cache-output-empty-base.ext4")
		if err := createEmptyDarwinVZCacheOutputExt4ImageFn(ctx, sourceRef, darwinVZCacheOutputVolumeMinimumBytes); err != nil {
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
	if strings.TrimSpace(path) == "" {
		return errors.New("missing empty cache output image path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create empty cache output image directory: %w", err)
	}
	sizeBytes := ext4image.AlignBytes(minimumBytes)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create empty cache output image %q: %w", path, err)
	}
	if err := file.Truncate(sizeBytes); err != nil {
		_ = file.Close()
		return fmt.Errorf("truncate empty cache output image %q to %d bytes: %w", path, sizeBytes, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close empty cache output image %q: %w", path, err)
	}
	mkfsPath, err := hosttools.ResolveE2FSProgsBinary("mkfs.ext4")
	if err != nil {
		return fmt.Errorf("find mkfs.ext4 for cache output volume: %w", err)
	}
	if out, err := exec.CommandContext(ctx, mkfsPath, "-q", "-F", path).CombinedOutput(); err != nil {
		_ = os.Remove(path)
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" {
			return fmt.Errorf("create empty cache output ext4 image: %w", err)
		}
		return fmt.Errorf("create empty cache output ext4 image: %w: %s", err, trimmed)
	}
	return nil
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
	if len(volumes) == 0 {
		return nil
	}
	mounts := make([]vsockexec.CacheOutputMount, 0, len(volumes))
	for _, volume := range volumes {
		mount := vsockexec.CacheOutputMount{
			DevicePath:    volume.DevicePath,
			MountPath:     volume.MountPath,
			SourcePresent: darwinVZCacheOutputSpecHasSource(volume.Spec),
		}
		for _, mapping := range volume.Spec.DirMappings {
			mount.DirMappings = append(mount.DirMappings, vsockexec.CacheOutputDirMount{
				GuestPath: strings.TrimSpace(mapping.GuestPath),
				Subpath:   strings.TrimSpace(mapping.Subpath),
			})
		}
		for _, mapping := range volume.Spec.FileMappings {
			mount.FileMappings = append(mount.FileMappings, vsockexec.CacheOutputFileMount{
				GuestPath: strings.TrimSpace(mapping.GuestPath),
				Subpath:   strings.TrimSpace(mapping.Subpath),
				Mode:      uint32(mapping.Mode.Perm()),
			})
		}
		mounts = append(mounts, mount)
	}
	return mounts
}

func darwinVZCacheOutputFileCaptures(volumes []preparedDarwinVZCacheOutputVolume, captures []backend.CacheOutputFileCapture) ([]vsockexec.CacheOutputFileCapture, error) {
	if len(captures) == 0 {
		return nil, nil
	}
	mountsByVolumeID := make(map[string]string, len(volumes))
	for _, volume := range volumes {
		mountsByVolumeID[strings.TrimSpace(volume.Spec.VolumeID)] = strings.TrimSpace(volume.MountPath)
	}
	out := make([]vsockexec.CacheOutputFileCapture, 0, len(captures))
	for i, capture := range captures {
		volumeID := strings.TrimSpace(capture.VolumeID)
		if volumeID == "" {
			return nil, fmt.Errorf("cache output file capture %d missing volume id", i)
		}
		mountPath, ok := mountsByVolumeID[volumeID]
		if !ok {
			return nil, fmt.Errorf("cache output file capture %d references unknown volume id %q", i, capture.VolumeID)
		}
		out = append(out, vsockexec.CacheOutputFileCapture{
			GuestPath: strings.TrimSpace(capture.GuestPath),
			MountPath: mountPath,
			Subpath:   strings.TrimSpace(capture.VolumeSubpath),
			Mode:      uint32(capture.Mode.Perm()),
		})
	}
	return out, nil
}

func cloneDarwinVZCacheOutputMounts(mounts []vsockexec.CacheOutputMount) []vsockexec.CacheOutputMount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]vsockexec.CacheOutputMount, len(mounts))
	for i, mount := range mounts {
		out[i] = mount
		out[i].DirMappings = append([]vsockexec.CacheOutputDirMount(nil), mount.DirMappings...)
		out[i].FileMappings = append([]vsockexec.CacheOutputFileMount(nil), mount.FileMappings...)
	}
	return out
}

func darwinVZCacheOutputSpecHasSource(spec backend.CacheOutputVolumeSpec) bool {
	return strings.TrimSpace(spec.SourceSnapshotRef) != "" || strings.TrimSpace(spec.StorageRef) != ""
}

func darwinVZCacheOutputRuntimeVolumeID(sandboxID, specVolumeID string, index int) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sandboxID) + "\x00" + strings.TrimSpace(specVolumeID)))
	return fmt.Sprintf("%s-cache-output-%02d-%s", strings.TrimSpace(sandboxID), index, hex.EncodeToString(sum[:6]))
}

func darwinVZCacheOutputGuestMountPath(index int) string {
	return filepath.Join(darwinVZCacheOutputGuestMountRoot, fmt.Sprintf("cacheout%d", index))
}

func darwinVZCacheOutputDevicePath(index int) string {
	return "/dev/" + darwinVZVirtioBlockDeviceName(index+1)
}

func darwinVZVirtioBlockDeviceName(index int) string {
	if index < 0 {
		index = 0
	}
	letters := ""
	for {
		letters = string(rune('a'+index%26)) + letters
		index = index/26 - 1
		if index < 0 {
			break
		}
	}
	return "vd" + letters
}
