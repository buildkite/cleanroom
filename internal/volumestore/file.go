package volumestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type FileDriverOptions struct {
	SnapshotBaseDir string
	Namespace       string
}

type FileDriver struct {
	*pathDriver
}

func NewFileDriver(opts FileDriverOptions) (*FileDriver, error) {
	driver, err := newPathDriver("file", opts.SnapshotBaseDir, opts.Namespace, copyFile)
	if err != nil {
		return nil, err
	}
	return &FileDriver{pathDriver: driver}, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Chmod(0o644); err != nil {
		return err
	}
	return out.Sync()
}

type pathDriver struct {
	name            string
	snapshotBaseDir string
	namespace       string
	copyFn          func(string, string) error
}

func newPathDriver(name, snapshotBaseDir, namespace string, copyFn func(string, string) error) (*pathDriver, error) {
	baseDir := strings.TrimSpace(snapshotBaseDir)
	if baseDir == "" {
		return nil, fmt.Errorf("%s volume driver requires snapshot base dir", name)
	}
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil, fmt.Errorf("%s volume driver requires namespace", name)
	}
	if copyFn == nil {
		return nil, fmt.Errorf("%s volume driver requires copy function", name)
	}
	return &pathDriver{
		name:            name,
		snapshotBaseDir: filepath.Clean(baseDir),
		namespace:       namespace,
		copyFn:          copyFn,
	}, nil
}

func (d *pathDriver) Name() string { return d.name }

func (d *pathDriver) EnsureBaseVolume(_ context.Context, req EnsureBaseVolumeRequest) (BaseVolume, error) {
	sourcePath := strings.TrimSpace(req.SourcePath)
	if sourcePath == "" {
		return BaseVolume{}, fmt.Errorf("%s volume driver requires source path", d.name)
	}
	resolved, err := filepath.Abs(sourcePath)
	if err != nil {
		return BaseVolume{}, fmt.Errorf("resolve base volume source %q: %w", sourcePath, err)
	}
	if _, err := os.Stat(resolved); err != nil {
		return BaseVolume{}, fmt.Errorf("inspect base volume source %q: %w", resolved, err)
	}
	return BaseVolume{Ref: resolved}, nil
}

func (d *pathDriver) CreateWritableVolume(_ context.Context, req CreateWritableVolumeRequest) (WritableVolume, error) {
	return d.copyToWritable(req.BaseRef, req.AttachmentPath)
}

func (d *pathDriver) SnapshotVolume(_ context.Context, req SnapshotVolumeRequest) (Snapshot, error) {
	volumeRef := strings.TrimSpace(req.VolumeRef)
	if volumeRef == "" {
		return Snapshot{}, fmt.Errorf("%s volume driver requires volume ref", d.name)
	}
	snapshotID := strings.TrimSpace(req.SnapshotID)
	if snapshotID == "" {
		return Snapshot{}, fmt.Errorf("%s volume driver requires snapshot id", d.name)
	}
	target := filepath.Join(d.snapshotBaseDir, d.namespace, snapshotID, "rootfs.ext4")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return Snapshot{}, fmt.Errorf("create snapshot directory %q: %w", filepath.Dir(target), err)
	}
	if err := d.copyFn(volumeRef, target); err != nil {
		_ = os.Remove(target)
		return Snapshot{}, fmt.Errorf("copy snapshot volume %q: %w", volumeRef, err)
	}
	return Snapshot{Ref: target, StorageRef: target}, nil
}

func (d *pathDriver) CloneSnapshotToVolume(_ context.Context, req CloneSnapshotToVolumeRequest) (WritableVolume, error) {
	return d.copyToWritable(req.SnapshotRef, req.AttachmentPath)
}

func (d *pathDriver) DestroyVolume(_ context.Context, req DestroyVolumeRequest) error {
	volumeRef := strings.TrimSpace(req.VolumeRef)
	if volumeRef == "" {
		return nil
	}
	if err := os.Remove(volumeRef); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove volume %q: %w", volumeRef, err)
	}
	return nil
}

func (d *pathDriver) DestroySnapshot(_ context.Context, req DestroySnapshotRequest) error {
	snapshotRef := strings.TrimSpace(req.SnapshotRef)
	if snapshotRef == "" {
		return nil
	}
	if err := os.Remove(snapshotRef); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove snapshot %q: %w", snapshotRef, err)
	}
	if dir := filepath.Dir(snapshotRef); dir != "" {
		_ = os.Remove(dir)
	}
	return nil
}

func (d *pathDriver) copyToWritable(sourceRef, attachmentPath string) (WritableVolume, error) {
	sourceRef = strings.TrimSpace(sourceRef)
	if sourceRef == "" {
		return WritableVolume{}, fmt.Errorf("%s volume driver requires source ref", d.name)
	}
	attachmentPath = strings.TrimSpace(attachmentPath)
	if attachmentPath == "" {
		return WritableVolume{}, fmt.Errorf("%s volume driver requires attachment path", d.name)
	}
	if err := os.MkdirAll(filepath.Dir(attachmentPath), 0o755); err != nil {
		return WritableVolume{}, fmt.Errorf("create writable volume directory %q: %w", filepath.Dir(attachmentPath), err)
	}
	if err := d.copyFn(sourceRef, attachmentPath); err != nil {
		return WritableVolume{}, fmt.Errorf("copy writable volume from %q: %w", sourceRef, err)
	}
	return WritableVolume{
		Ref:            attachmentPath,
		AttachmentPath: attachmentPath,
	}, nil
}
