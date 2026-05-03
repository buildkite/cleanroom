package firecracker

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
	charmlog "github.com/charmbracelet/log"
)

const defaultCacheOutputVolumeMinimumBytes int64 = 512 << 20
const cacheOutputGuestMountRoot = "/run/cleanroom/cache-output-volumes"

var (
	cacheOutputVolumeMinimumBytes     = defaultCacheOutputVolumeMinimumBytes
	createEmptyCacheOutputExt4ImageFn = createEmptyCacheOutputExt4Image
)

type preparedCacheOutputVolume struct {
	Spec   backend.CacheOutputVolumeSpec
	Drive  drive
	Volume volumestore.WritableVolume
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
		if err := validateCacheOutputVolumeSpec(spec); err != nil {
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
		volume, volumeCleanup, err := prepareWritableVolumeWithLogger(ctx, logger, volumeCfg, runtimeVolumeID, attachmentPath, sourceRef, cacheOutputVolumeMinimumBytes)
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

func validateCacheOutputVolumeSpec(spec backend.CacheOutputVolumeSpec) error {
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

func cacheOutputVolumeSource(ctx context.Context, cfg backend.FirecrackerConfig, runDir string, spec backend.CacheOutputVolumeSpec) (sourceRef string, volumeCfg backend.FirecrackerConfig, err error) {
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
		if err := createEmptyCacheOutputExt4ImageFn(ctx, sourceRef, cacheOutputVolumeMinimumBytes); err != nil {
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
	if strings.TrimSpace(path) == "" {
		return errors.New("missing empty cache output image path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create empty cache output image directory: %w", err)
	}
	sizeBytes := ext4image.AlignBytes(minimumBytes)
	if err := os.Truncate(path, sizeBytes); err != nil {
		return fmt.Errorf("truncate empty cache output image %q to %d bytes: %w", path, sizeBytes, err)
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
	return fmt.Sprintf("cacheout%d", index)
}

func cacheOutputRuntimeVolumeID(sandboxID, specVolumeID string, index int) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sandboxID) + "\x00" + strings.TrimSpace(specVolumeID)))
	return fmt.Sprintf("%s-cache-output-%02d-%s", strings.TrimSpace(sandboxID), index, hex.EncodeToString(sum[:6]))
}

func cacheOutputVolumeMounts(volumes []preparedCacheOutputVolume) []vsockexec.CacheOutputMount {
	if len(volumes) == 0 {
		return nil
	}
	mounts := make([]vsockexec.CacheOutputMount, 0, len(volumes))
	for i, volume := range volumes {
		mount := vsockexec.CacheOutputMount{
			DevicePath:    cacheOutputDevicePath(i),
			MountPath:     cacheOutputGuestMountPath(volume.Drive.DriveID),
			SourcePresent: cacheOutputSpecHasSource(volume.Spec),
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

func cloneCacheOutputMounts(mounts []vsockexec.CacheOutputMount) []vsockexec.CacheOutputMount {
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
	return filepath.Join(cacheOutputGuestMountRoot, strings.TrimSpace(driveID))
}

func cacheOutputSpecHasSource(spec backend.CacheOutputVolumeSpec) bool {
	return strings.TrimSpace(spec.SourceSnapshotRef) != "" || strings.TrimSpace(spec.StorageRef) != ""
}

func cacheOutputDevicePath(index int) string {
	return "/dev/" + virtioBlockDeviceName(index+1)
}

func virtioBlockDeviceName(index int) string {
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
