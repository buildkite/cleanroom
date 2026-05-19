package cacheoutput

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
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

const DefaultVolumeMinimumBytes int64 = 16 << 30
const GuestMountRoot = "/run/cleanroom/cache-output-volumes"

type PreparedMount struct {
	Spec       backend.CacheOutputVolumeSpec
	DevicePath string
	MountPath  string
}

func ValidateVolumeSpec(spec backend.CacheOutputVolumeSpec) error {
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

func SourceRef(spec backend.CacheOutputVolumeSpec) string {
	if sourceRef := strings.TrimSpace(spec.SourceSnapshotRef); sourceRef != "" {
		return sourceRef
	}
	return strings.TrimSpace(spec.StorageRef)
}

func SpecHasSource(spec backend.CacheOutputVolumeSpec) bool {
	return SourceRef(spec) != ""
}

func CreateEmptyExt4Image(ctx context.Context, path string, minimumBytes int64) error {
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

func RuntimeVolumeID(sandboxID, specVolumeID string, index int) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sandboxID) + "\x00" + strings.TrimSpace(specVolumeID)))
	return fmt.Sprintf("%s-cache-output-%02d-%s", strings.TrimSpace(sandboxID), index, hex.EncodeToString(sum[:6]))
}

func MountID(index int) string {
	return fmt.Sprintf("cacheout%d", index)
}

func GuestMountPath(root, mountID string) string {
	return filepath.Join(strings.TrimSpace(root), strings.TrimSpace(mountID))
}

func DevicePathAfterRoot(index int) string {
	return "/dev/" + VirtioBlockDeviceName(index+1)
}

func VirtioBlockDeviceName(index int) string {
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

func Mounts(volumes []PreparedMount) []vsockexec.CacheOutputMount {
	if len(volumes) == 0 {
		return nil
	}
	mounts := make([]vsockexec.CacheOutputMount, 0, len(volumes))
	for _, volume := range volumes {
		mount := vsockexec.CacheOutputMount{
			DevicePath:    strings.TrimSpace(volume.DevicePath),
			MountPath:     strings.TrimSpace(volume.MountPath),
			SourcePresent: SpecHasSource(volume.Spec),
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

func FileCaptures(volumes []PreparedMount, captures []backend.CacheOutputFileCapture) ([]vsockexec.CacheOutputFileCapture, error) {
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

func CloneMounts(mounts []vsockexec.CacheOutputMount) []vsockexec.CacheOutputMount {
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

func SnapshotID(prefix, volumeID string) string {
	hash := SHA256String(strings.TrimSpace(volumeID))
	return strings.TrimSpace(prefix) + "-" + hash[:16]
}

func SnapshotOutputs(spec backend.CacheOutputVolumeSpec) []backend.CacheOutputVolumeSnapshotOutput {
	out := make([]backend.CacheOutputVolumeSnapshotOutput, 0, len(spec.DirMappings)+len(spec.FileMappings))
	for _, mapping := range spec.DirMappings {
		out = append(out, backend.CacheOutputVolumeSnapshotOutput{
			Kind:          "dir",
			GuestPath:     strings.TrimSpace(mapping.GuestPath),
			VolumeSubpath: strings.TrimSpace(mapping.Subpath),
		})
	}
	for _, mapping := range spec.FileMappings {
		out = append(out, backend.CacheOutputVolumeSnapshotOutput{
			Kind:          "file",
			GuestPath:     strings.TrimSpace(mapping.GuestPath),
			VolumeSubpath: strings.TrimSpace(mapping.Subpath),
			Mode:          mapping.Mode.Perm(),
		})
	}
	return out
}

func SelectByVolumeID[T any](volumes []T, volumeIDs []string, volumeID func(T) string) ([]T, error) {
	if len(volumeIDs) == 0 {
		return append([]T(nil), volumes...), nil
	}
	byID := make(map[string]T, len(volumes))
	for _, volume := range volumes {
		byID[strings.TrimSpace(volumeID(volume))] = volume
	}
	selected := make([]T, 0, len(volumeIDs))
	seen := make(map[string]struct{}, len(volumeIDs))
	for _, id := range volumeIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, errors.New("cache output volume id cannot be empty")
		}
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("duplicate cache output volume id %q", id)
		}
		seen[id] = struct{}{}
		volume, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("unknown cache output volume id %q", id)
		}
		selected = append(selected, volume)
	}
	return selected, nil
}

func SHA256String(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
