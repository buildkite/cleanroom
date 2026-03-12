package volumestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const zvolDeviceWaitTimeout = 10 * time.Second
const zvolDeviceWaitInterval = 50 * time.Millisecond

const zfsBaseSnapshotName = "seed"

type ZFSCommandRunner interface {
	Run(ctx context.Context, command string, args ...string) error
	Output(ctx context.Context, command string, args ...string) ([]byte, error)
}

type ZFSDriverOptions struct {
	DatasetRoot string
	Runner      ZFSCommandRunner
	Stat        func(string) (os.FileInfo, error)
}

type ZFSDriver struct {
	datasetRoot string
	runner      ZFSCommandRunner
	stat        func(string) (os.FileInfo, error)
}

func NewZFSDriver(opts ZFSDriverOptions) (*ZFSDriver, error) {
	datasetRoot := strings.Trim(strings.TrimSpace(opts.DatasetRoot), "/")
	if datasetRoot == "" {
		return nil, errors.New("zfs volume driver requires dataset root")
	}
	if opts.Runner == nil {
		return nil, errors.New("zfs volume driver requires command runner")
	}
	statFn := opts.Stat
	if statFn == nil {
		statFn = os.Stat
	}
	return &ZFSDriver{
		datasetRoot: datasetRoot,
		runner:      opts.Runner,
		stat:        statFn,
	}, nil
}

func (d *ZFSDriver) Name() string { return "zfs" }

func (d *ZFSDriver) EnsureBaseVolume(ctx context.Context, req EnsureBaseVolumeRequest) (BaseVolume, error) {
	sourcePath := strings.TrimSpace(req.SourcePath)
	if sourcePath == "" {
		return BaseVolume{}, errors.New("zfs volume driver requires source path")
	}
	info, err := d.stat(sourcePath)
	if err != nil {
		return BaseVolume{}, fmt.Errorf("inspect base volume source %q: %w", sourcePath, err)
	}
	if info.Size() <= 0 {
		return BaseVolume{}, fmt.Errorf("inspect base volume source %q: empty file", sourcePath)
	}

	baseDataset := d.datasetPath("base", sanitizeZFSDatasetComponent(req.BaseID))
	baseSnapshot := d.snapshotRef(baseDataset, zfsBaseSnapshotName)

	baseExists, err := d.refExists(ctx, baseDataset)
	if err != nil {
		return BaseVolume{}, err
	}
	if !baseExists {
		if err := d.runner.Run(ctx, "zfs", "create", "-p", "-V", strconv.FormatInt(info.Size(), 10), baseDataset); err != nil {
			return BaseVolume{}, fmt.Errorf("create zfs base volume %q: %w", baseDataset, err)
		}
		if err := d.waitForZvolDevice(ctx, baseDataset); err != nil {
			_ = d.runner.Run(context.Background(), "zfs", "destroy", "-r", baseDataset)
			return BaseVolume{}, fmt.Errorf("wait for zfs base volume device %q: %w", baseDataset, err)
		}
		if err := d.runner.Run(ctx, "dd", "if="+sourcePath, "of="+zvolDevicePath(baseDataset), "bs=4M", "conv=fsync", "status=none"); err != nil {
			_ = d.runner.Run(context.Background(), "zfs", "destroy", "-r", baseDataset)
			return BaseVolume{}, fmt.Errorf("seed zfs base volume %q: %w", baseDataset, err)
		}
	}

	snapshotExists, err := d.refExists(ctx, baseSnapshot)
	if err != nil {
		return BaseVolume{}, err
	}
	if !snapshotExists {
		if err := d.runner.Run(ctx, "zfs", "snapshot", baseSnapshot); err != nil {
			return BaseVolume{}, fmt.Errorf("create zfs base snapshot %q: %w", baseSnapshot, err)
		}
	}
	return BaseVolume{Ref: baseSnapshot}, nil
}

func (d *ZFSDriver) CreateWritableVolume(ctx context.Context, req CreateWritableVolumeRequest) (WritableVolume, error) {
	baseRef := strings.TrimSpace(req.BaseRef)
	if baseRef == "" {
		return WritableVolume{}, errors.New("zfs volume driver requires base ref")
	}
	if !strings.Contains(baseRef, "@") {
		return WritableVolume{}, fmt.Errorf("zfs base ref %q must be a snapshot", baseRef)
	}
	dataset := d.datasetPath("sandboxes", sanitizeZFSDatasetComponent(req.VolumeID))
	return d.cloneSnapshot(ctx, baseRef, dataset)
}

func (d *ZFSDriver) SnapshotVolume(ctx context.Context, req SnapshotVolumeRequest) (Snapshot, error) {
	volumeRef := strings.TrimSpace(req.VolumeRef)
	if volumeRef == "" {
		return Snapshot{}, errors.New("zfs volume driver requires volume ref")
	}
	snapshotID := sanitizeZFSDatasetComponent(req.SnapshotID)
	if snapshotID == "" {
		return Snapshot{}, errors.New("zfs volume driver requires snapshot id")
	}
	snapshotRef := d.snapshotRef(volumeRef, "snap-"+snapshotID)
	if err := d.runner.Run(ctx, "zfs", "snapshot", snapshotRef); err != nil {
		return Snapshot{}, fmt.Errorf("create zfs snapshot %q: %w", snapshotRef, err)
	}
	return Snapshot{Ref: snapshotRef, StorageRef: snapshotRef}, nil
}

func (d *ZFSDriver) CloneSnapshotToVolume(ctx context.Context, req CloneSnapshotToVolumeRequest) (WritableVolume, error) {
	snapshotRef := strings.TrimSpace(req.SnapshotRef)
	if snapshotRef == "" {
		return WritableVolume{}, errors.New("zfs volume driver requires snapshot ref")
	}
	if !strings.Contains(snapshotRef, "@") {
		return WritableVolume{}, fmt.Errorf("zfs snapshot ref %q must be a snapshot", snapshotRef)
	}
	dataset := d.datasetPath("sandboxes", sanitizeZFSDatasetComponent(req.VolumeID))
	return d.cloneSnapshot(ctx, snapshotRef, dataset)
}

func (d *ZFSDriver) DestroyVolume(ctx context.Context, req DestroyVolumeRequest) error {
	volumeRef := strings.TrimSpace(req.VolumeRef)
	if volumeRef == "" {
		return nil
	}
	if err := d.runner.Run(ctx, "zfs", "destroy", "-r", volumeRef); err != nil {
		if isZFSMissingError(err) {
			return nil
		}
		return fmt.Errorf("destroy zfs volume %q: %w", volumeRef, err)
	}
	return nil
}

func (d *ZFSDriver) DestroySnapshot(ctx context.Context, req DestroySnapshotRequest) error {
	snapshotRef := strings.TrimSpace(req.SnapshotRef)
	if snapshotRef == "" {
		return nil
	}
	if err := d.runner.Run(ctx, "zfs", "destroy", snapshotRef); err != nil {
		if isZFSMissingError(err) {
			return nil
		}
		return fmt.Errorf("destroy zfs snapshot %q: %w", snapshotRef, err)
	}
	return nil
}

func (d *ZFSDriver) datasetPath(parts ...string) string {
	items := []string{d.datasetRoot}
	for _, part := range parts {
		part = strings.Trim(strings.TrimSpace(part), "/")
		if part == "" {
			continue
		}
		items = append(items, part)
	}
	return strings.Join(items, "/")
}

func (d *ZFSDriver) snapshotRef(dataset, snapshotName string) string {
	return dataset + "@" + sanitizeZFSDatasetComponent(snapshotName)
}

func (d *ZFSDriver) cloneSnapshot(ctx context.Context, snapshotRef, dataset string) (WritableVolume, error) {
	exists, err := d.refExists(ctx, dataset)
	if err != nil {
		return WritableVolume{}, err
	}
	if exists {
		return WritableVolume{}, fmt.Errorf("zfs dataset %q already exists", dataset)
	}
	if err := d.runner.Run(ctx, "zfs", "clone", "-p", snapshotRef, dataset); err != nil {
		return WritableVolume{}, fmt.Errorf("clone zfs snapshot %q into %q: %w", snapshotRef, dataset, err)
	}
	if err := d.waitForZvolDevice(ctx, dataset); err != nil {
		_ = d.runner.Run(context.Background(), "zfs", "destroy", "-r", dataset)
		return WritableVolume{}, fmt.Errorf("wait for cloned zvol device %q: %w", dataset, err)
	}
	return WritableVolume{
		Ref:            dataset,
		AttachmentPath: zvolDevicePath(dataset),
	}, nil
}

func (d *ZFSDriver) refExists(ctx context.Context, ref string) (bool, error) {
	out, err := d.runner.Output(ctx, "zfs", "list", "-H", "-o", "name", ref)
	if err != nil {
		if isZFSMissingError(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect zfs ref %q: %w", ref, err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func sanitizeZFSDatasetComponent(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(value))
	lastDash := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' || r == ':' {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "cleanroom"
	}
	return out
}

func zvolDevicePath(dataset string) string {
	return filepath.Join(append([]string{"/dev/zvol"}, strings.Split(strings.TrimSpace(dataset), "/")...)...)
}

func (d *ZFSDriver) waitForZvolDevice(ctx context.Context, dataset string) error {
	devicePath := zvolDevicePath(dataset)
	deadline := time.Now().Add(zvolDeviceWaitTimeout)
	for {
		if _, err := d.stat(devicePath); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for zvol device %q", devicePath)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("context canceled waiting for zvol device %q: %w", devicePath, ctx.Err())
		}
		time.Sleep(zvolDeviceWaitInterval)
	}
}

func isZFSMissingError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "dataset does not exist") ||
		strings.Contains(msg, "no such pool or dataset") ||
		strings.Contains(msg, "snapshot does not exist") ||
		strings.Contains(msg, "could not find any snapshots to destroy")
}
