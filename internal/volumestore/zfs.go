package volumestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

const zfsManagedSnapshotName = "base"
const zfsBaseNamespace = "base"
const zfsSandboxNamespace = "sandboxes"
const zfsSnapshotNamespace = "snapshots"
const zfsSnapshotImportNamespace = "imports"

type ZFSCommandRunner interface {
	Run(ctx context.Context, command string, args ...string) error
	Output(ctx context.Context, command string, args ...string) ([]byte, error)
}

type ZFSCommandOutputStreamer interface {
	OutputTo(ctx context.Context, dst io.Writer, command string, args ...string) error
}

type ZFSCommandInputStreamer interface {
	InputFrom(ctx context.Context, src io.Reader, command string, args ...string) error
}

type ZFSDriverOptions struct {
	DatasetRoot             string
	Runner                  ZFSCommandRunner
	Stat                    func(string) (os.FileInfo, error)
	DisableSnapshotMetadata bool
}

type ZFSDriver struct {
	datasetRoot             string
	runner                  ZFSCommandRunner
	stat                    func(string) (os.FileInfo, error)
	disableSnapshotMetadata bool
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
		datasetRoot:             datasetRoot,
		runner:                  opts.Runner,
		stat:                    statFn,
		disableSnapshotMetadata: opts.DisableSnapshotMetadata,
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

	baseDataset := d.baseDataset(req.BaseID, req.MinimumBytes)
	baseSnapshot := d.snapshotRef(baseDataset, zfsManagedSnapshotName)
	volumeSize := info.Size()
	if req.MinimumBytes > volumeSize {
		volumeSize = req.MinimumBytes
	}

	baseExists, err := d.refExists(ctx, baseDataset)
	if err != nil {
		return BaseVolume{}, err
	}
	if !baseExists {
		if err := d.runner.Run(ctx, "zfs", "create", "-p", "-V", strconv.FormatInt(volumeSize, 10), baseDataset); err != nil {
			return BaseVolume{}, fmt.Errorf("create zfs base volume %q: %w", baseDataset, err)
		}
		if err := d.runner.Run(ctx, "dd", "if="+sourcePath, "of="+zvolDevicePath(baseDataset), "bs=4M", "conv=fsync", "status=none"); err != nil {
			_ = d.runner.Run(context.Background(), "zfs", "destroy", "-r", baseDataset)
			return BaseVolume{}, fmt.Errorf("initialize zfs base volume %q: %w", baseDataset, err)
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

	dataset := d.datasetPath(zfsSandboxNamespace, sanitizeZFSDatasetComponent(req.VolumeID))
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
	var parentSnapshotGUID string
	if !d.disableSnapshotMetadata {
		var err error
		parentSnapshotGUID, err = d.parentSnapshotGUIDForVolume(ctx, volumeRef)
		if err != nil {
			return Snapshot{}, err
		}
	}

	sourceSnapshotRef := d.snapshotRef(volumeRef, "snap-"+snapshotID)
	storedDataset := d.datasetPath(zfsSnapshotNamespace, snapshotID)
	storedSnapshotRef := d.snapshotRef(storedDataset, zfsManagedSnapshotName)

	exists, err := d.refExists(ctx, storedDataset)
	if err != nil {
		return Snapshot{}, err
	}
	if exists {
		return Snapshot{}, fmt.Errorf("zfs snapshot dataset %q already exists", storedDataset)
	}

	if err := d.runner.Run(ctx, "zfs", "snapshot", sourceSnapshotRef); err != nil {
		return Snapshot{}, fmt.Errorf("create zfs snapshot %q: %w", sourceSnapshotRef, err)
	}
	sourceSnapshotCreated := true
	defer func() {
		if !sourceSnapshotCreated {
			return
		}
		_ = d.DestroySnapshot(context.Background(), DestroySnapshotRequest{SnapshotRef: sourceSnapshotRef})
	}()

	if _, err := d.cloneSnapshot(ctx, sourceSnapshotRef, storedDataset); err != nil {
		return Snapshot{}, err
	}
	storedDatasetCreated := true
	defer func() {
		if !storedDatasetCreated {
			return
		}
		_ = d.DestroyVolume(context.Background(), DestroyVolumeRequest{VolumeRef: storedDataset})
	}()

	if err := d.runner.Run(ctx, "zfs", "promote", storedDataset); err != nil {
		return Snapshot{}, fmt.Errorf("promote zfs dataset %q: %w", storedDataset, err)
	}

	if err := d.runner.Run(ctx, "zfs", "snapshot", storedSnapshotRef); err != nil {
		return Snapshot{}, fmt.Errorf("create zfs snapshot %q: %w", storedSnapshotRef, err)
	}
	storedDatasetCreated = false

	return Snapshot{Ref: storedSnapshotRef, StorageRef: storedSnapshotRef, ParentSnapshotGUID: parentSnapshotGUID}, nil
}

func (d *ZFSDriver) CloneSnapshotToVolume(ctx context.Context, req CloneSnapshotToVolumeRequest) (WritableVolume, error) {
	snapshotRef := strings.TrimSpace(req.SnapshotRef)
	if snapshotRef == "" {
		return WritableVolume{}, errors.New("zfs volume driver requires snapshot ref")
	}
	if !strings.Contains(snapshotRef, "@") {
		return WritableVolume{}, fmt.Errorf("zfs snapshot ref %q must be a snapshot", snapshotRef)
	}

	dataset := d.datasetPath(zfsSandboxNamespace, sanitizeZFSDatasetComponent(req.VolumeID))
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

	command := []string{"zfs", "destroy", snapshotRef}
	if dataset, ok := storedZFSSnapshotDataset(snapshotRef); ok {
		command = []string{"zfs", "destroy", "-r", dataset}
	}
	if err := d.runner.Run(ctx, command[0], command[1:]...); err != nil {
		if isZFSMissingError(err) {
			return nil
		}
		return fmt.Errorf("destroy zfs snapshot %q: %w", snapshotRef, err)
	}
	return nil
}

func (d *ZFSDriver) EnsureWritableVolumeMinimumSize(ctx context.Context, volume WritableVolume, minimumBytes int64) error {
	if minimumBytes <= 0 {
		return nil
	}

	currentBytes, isBlockDevice, err := ext4imagePathSizeBytes(volume.AttachmentPath)
	if err != nil {
		return err
	}
	if isBlockDevice && currentBytes < minimumBytes {
		targetBytes := ext4imageAlignBytes(minimumBytes)
		if err := d.runner.Run(ctx, "zfs", "set", "volsize="+strconv.FormatInt(targetBytes, 10), volume.Ref); err != nil {
			return fmt.Errorf("grow zfs volume %q to %d bytes: %w", volume.Ref, targetBytes, err)
		}
	}
	return ext4imageEnsureMinimumSize(ctx, volume.AttachmentPath, minimumBytes)
}

func (d *ZFSDriver) DescribeSnapshot(ctx context.Context, req DescribeSnapshotRequest) (SnapshotDescription, error) {
	snapshotRef := strings.TrimSpace(req.SnapshotRef)
	if snapshotRef == "" {
		snapshotRef = strings.TrimSpace(req.StorageRef)
	}
	if err := d.validateManagedSnapshotRef(snapshotRef); err != nil {
		return SnapshotDescription{}, err
	}
	guid, err := d.snapshotGUID(ctx, snapshotRef)
	if err != nil {
		return SnapshotDescription{}, err
	}
	return SnapshotDescription{
		SnapshotRef:        snapshotRef,
		StorageRef:         snapshotRef,
		SnapshotGUID:       guid,
		ParentSnapshotGUID: strings.TrimSpace(req.ParentSnapshotGUID),
	}, nil
}

func (d *ZFSDriver) PlanIncrementalSnapshotExport(ctx context.Context, req IncrementalSnapshotExportRequest) (IncrementalSnapshotExportPlan, error) {
	expectedFromGUID := strings.TrimSpace(req.FromSnapshotGUID)
	if expectedFromGUID == "" {
		return IncrementalSnapshotExportPlan{}, errors.New("zfs incremental export requires from snapshot guid")
	}

	from, err := d.DescribeSnapshot(ctx, DescribeSnapshotRequest{SnapshotRef: req.FromSnapshotRef})
	if err != nil {
		return IncrementalSnapshotExportPlan{}, fmt.Errorf("describe zfs incremental parent snapshot: %w", err)
	}
	if from.SnapshotGUID != expectedFromGUID {
		return IncrementalSnapshotExportPlan{}, fmt.Errorf("zfs incremental parent guid mismatch for %q: got %q want %q", from.SnapshotRef, from.SnapshotGUID, expectedFromGUID)
	}

	to, err := d.DescribeSnapshot(ctx, DescribeSnapshotRequest{SnapshotRef: req.ToSnapshotRef})
	if err != nil {
		return IncrementalSnapshotExportPlan{}, fmt.Errorf("describe zfs incremental child snapshot: %w", err)
	}
	expectedToGUID := strings.TrimSpace(req.ToSnapshotGUID)
	if expectedToGUID != "" && to.SnapshotGUID != expectedToGUID {
		return IncrementalSnapshotExportPlan{}, fmt.Errorf("zfs incremental child guid mismatch for %q: got %q want %q", to.SnapshotRef, to.SnapshotGUID, expectedToGUID)
	}

	estimatedBytes, err := d.estimateIncrementalSend(ctx, from.SnapshotRef, to.SnapshotRef)
	if err != nil {
		return IncrementalSnapshotExportPlan{}, err
	}
	return IncrementalSnapshotExportPlan{
		FromSnapshotRef:  from.SnapshotRef,
		FromSnapshotGUID: from.SnapshotGUID,
		ToSnapshotRef:    to.SnapshotRef,
		ToSnapshotGUID:   to.SnapshotGUID,
		EstimatedBytes:   estimatedBytes,
	}, nil
}

func (d *ZFSDriver) ExportIncrementalSnapshot(ctx context.Context, plan IncrementalSnapshotExportPlan, dst io.Writer) error {
	if dst == nil {
		return errors.New("zfs incremental export requires output writer")
	}
	validated, err := d.PlanIncrementalSnapshotExport(ctx, IncrementalSnapshotExportRequest{
		FromSnapshotRef:  plan.FromSnapshotRef,
		FromSnapshotGUID: plan.FromSnapshotGUID,
		ToSnapshotRef:    plan.ToSnapshotRef,
		ToSnapshotGUID:   plan.ToSnapshotGUID,
	})
	if err != nil {
		return err
	}

	streamer, ok := d.runner.(ZFSCommandOutputStreamer)
	if !ok {
		return errors.New("zfs command runner does not support streaming output")
	}
	if err := streamer.OutputTo(ctx, dst, "zfs", "send", "-i", validated.FromSnapshotRef, validated.ToSnapshotRef); err != nil {
		return fmt.Errorf("export zfs incremental send from %q to %q: %w", validated.FromSnapshotRef, validated.ToSnapshotRef, err)
	}
	return nil
}

func (d *ZFSDriver) ImportIncrementalSnapshot(ctx context.Context, req IncrementalSnapshotImportRequest, src io.Reader) (Snapshot, error) {
	if src == nil {
		return Snapshot{}, errors.New("zfs incremental import requires input reader")
	}
	snapshotID := sanitizeZFSDatasetComponent(req.SnapshotID)
	if snapshotID == "" {
		return Snapshot{}, errors.New("zfs incremental import requires snapshot id")
	}
	parentSnapshotRef := strings.TrimSpace(req.ParentSnapshotRef)
	if err := d.validateManagedSnapshotRef(parentSnapshotRef); err != nil {
		return Snapshot{}, err
	}
	expectedParentGUID := strings.TrimSpace(req.ParentSnapshotGUID)
	if expectedParentGUID == "" {
		return Snapshot{}, errors.New("zfs incremental import requires parent snapshot guid")
	}

	parent, err := d.DescribeSnapshot(ctx, DescribeSnapshotRequest{SnapshotRef: parentSnapshotRef})
	if err != nil {
		return Snapshot{}, fmt.Errorf("describe zfs incremental import parent snapshot: %w", err)
	}
	if parent.SnapshotGUID != expectedParentGUID {
		return Snapshot{}, fmt.Errorf("zfs incremental import parent guid mismatch for %q: got %q want %q", parent.SnapshotRef, parent.SnapshotGUID, expectedParentGUID)
	}

	streamer, ok := d.runner.(ZFSCommandInputStreamer)
	if !ok {
		return Snapshot{}, errors.New("zfs command runner does not support streaming input")
	}

	storedDataset := d.importDataset(snapshotID)
	storedSnapshotRef := d.snapshotRef(storedDataset, zfsManagedSnapshotName)
	if _, err := d.cloneSnapshot(ctx, parent.SnapshotRef, storedDataset); err != nil {
		return Snapshot{}, err
	}
	importComplete := false
	defer func() {
		if importComplete {
			return
		}
		_ = d.DestroyVolume(context.Background(), DestroyVolumeRequest{VolumeRef: storedDataset})
	}()

	if err := streamer.InputFrom(ctx, src, "zfs", "receive", "-u", "-F", storedDataset); err != nil {
		return Snapshot{}, fmt.Errorf("receive zfs incremental snapshot into %q: %w", storedDataset, err)
	}

	desc, err := d.DescribeSnapshot(ctx, DescribeSnapshotRequest{
		SnapshotRef:        storedSnapshotRef,
		ParentSnapshotGUID: parent.SnapshotGUID,
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("describe imported zfs snapshot: %w", err)
	}
	expectedSnapshotGUID := strings.TrimSpace(req.ExpectedSnapshotGUID)
	if expectedSnapshotGUID != "" && desc.SnapshotGUID != expectedSnapshotGUID {
		return Snapshot{}, fmt.Errorf("zfs incremental import snapshot guid mismatch for %q: got %q want %q", desc.SnapshotRef, desc.SnapshotGUID, expectedSnapshotGUID)
	}

	driverMetadata, err := EncodeZFSDriverMetadata(ZFSDriverMetadataFromDescription(desc))
	if err != nil {
		return Snapshot{}, err
	}
	importComplete = true
	return Snapshot{
		Ref:                desc.SnapshotRef,
		StorageRef:         desc.StorageRef,
		ParentSnapshotGUID: desc.ParentSnapshotGUID,
		StorageSizeBytes:   desc.StorageSizeBytes,
		ExclusiveSizeBytes: desc.ExclusiveSizeBytes,
		DriverMetadata:     driverMetadata,
	}, nil
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

func (d *ZFSDriver) validateManagedSnapshotRef(snapshotRef string) error {
	snapshotRef = strings.TrimSpace(snapshotRef)
	if snapshotRef == "" {
		return errors.New("zfs snapshot ref is required")
	}
	if !strings.Contains(snapshotRef, "@") {
		return fmt.Errorf("zfs ref %q must be a snapshot", snapshotRef)
	}
	root, ok := ZFSDatasetRootFromManagedRef(snapshotRef)
	if !ok {
		return fmt.Errorf("zfs snapshot ref %q is not a managed cleanroom ref", snapshotRef)
	}
	if root != d.datasetRoot {
		return fmt.Errorf("zfs snapshot ref %q belongs to dataset root %q, expected %q", snapshotRef, root, d.datasetRoot)
	}
	return nil
}

func (d *ZFSDriver) snapshotGUID(ctx context.Context, snapshotRef string) (string, error) {
	out, err := d.runner.Output(ctx, "zfs", "get", "-H", "-o", "value", "guid", snapshotRef)
	if err != nil {
		return "", fmt.Errorf("read zfs snapshot guid for %q: %w", snapshotRef, err)
	}
	guid := strings.TrimSpace(string(out))
	if guid == "" || guid == "-" {
		return "", fmt.Errorf("zfs snapshot %q has no guid", snapshotRef)
	}
	return guid, nil
}

func (d *ZFSDriver) parentSnapshotGUIDForVolume(ctx context.Context, volumeRef string) (string, error) {
	origin, err := d.datasetOriginSnapshotRef(ctx, volumeRef)
	if err != nil {
		return "", err
	}
	if origin == "" {
		return "", nil
	}
	return d.snapshotGUID(ctx, origin)
}

func (d *ZFSDriver) datasetOriginSnapshotRef(ctx context.Context, dataset string) (string, error) {
	dataset = strings.TrimSpace(dataset)
	if dataset == "" {
		return "", errors.New("zfs dataset ref is required")
	}
	root, ok := ZFSDatasetRootFromManagedRef(dataset)
	if !ok {
		return "", fmt.Errorf("zfs dataset ref %q is not a managed cleanroom ref", dataset)
	}
	if root != d.datasetRoot {
		return "", fmt.Errorf("zfs dataset ref %q belongs to dataset root %q, expected %q", dataset, root, d.datasetRoot)
	}
	out, err := d.runner.Output(ctx, "zfs", "get", "-H", "-o", "value", "origin", dataset)
	if err != nil {
		return "", fmt.Errorf("read zfs origin for %q: %w", dataset, err)
	}
	origin := strings.TrimSpace(string(out))
	if origin == "" || origin == "-" {
		return "", nil
	}
	if err := d.validateManagedSnapshotRef(origin); err != nil {
		return "", fmt.Errorf("read zfs origin for %q: %w", dataset, err)
	}
	return origin, nil
}

func (d *ZFSDriver) estimateIncrementalSend(ctx context.Context, fromSnapshotRef, toSnapshotRef string) (int64, error) {
	out, err := d.runner.Output(ctx, "zfs", "send", "-nP", "-i", fromSnapshotRef, toSnapshotRef)
	if err != nil {
		return 0, fmt.Errorf("plan zfs incremental send from %q to %q: %w", fromSnapshotRef, toSnapshotRef, err)
	}
	return parseZFSSendEstimateBytes(out), nil
}

func (d *ZFSDriver) baseDataset(baseID string, minimumBytes int64) string {
	baseID = sanitizeZFSDatasetComponent(baseID)
	if minimumBytes > 0 {
		baseID = fmt.Sprintf("%s-min-%d", baseID, minimumBytes)
	}
	return d.datasetPath(zfsBaseNamespace, baseID)
}

func (d *ZFSDriver) snapshotRef(dataset, snapshotName string) string {
	return dataset + "@" + sanitizeZFSDatasetComponent(snapshotName)
}

func (d *ZFSDriver) importDataset(snapshotID string) string {
	return d.datasetPath(zfsSnapshotNamespace, zfsSnapshotImportNamespace, snapshotID)
}

func storedZFSSnapshotDataset(snapshotRef string) (string, bool) {
	dataset := datasetFromSnapshotRef(snapshotRef)
	if dataset == "" {
		return "", false
	}
	marker := "/" + zfsSnapshotNamespace + "/"
	idx := strings.LastIndex(dataset, marker)
	if idx <= 0 {
		return "", false
	}
	if strings.TrimSpace(dataset[idx+len(marker):]) == "" {
		return "", false
	}
	return dataset, true
}

func ZFSDatasetRootFromStoredSnapshotRef(snapshotRef string) (string, bool) {
	dataset, ok := storedZFSSnapshotDataset(snapshotRef)
	if !ok {
		return "", false
	}

	marker := "/" + zfsSnapshotNamespace + "/"
	idx := strings.LastIndex(dataset, marker)
	if idx <= 0 {
		return "", false
	}
	root := strings.TrimSpace(dataset[:idx])
	if root == "" {
		return "", false
	}
	return root, true
}

func ZFSDatasetRootFromManagedRef(ref string) (string, bool) {
	dataset := strings.TrimSpace(ref)
	if dataset == "" {
		return "", false
	}
	if strings.Contains(dataset, "@") {
		dataset = datasetFromSnapshotRef(dataset)
	}
	if dataset == "" {
		return "", false
	}

	components := strings.Split(dataset, "/")
	if len(components) < 3 {
		return "", false
	}
	for _, component := range components {
		if strings.TrimSpace(component) == "" {
			return "", false
		}
	}

	var rootComponents []string
	if len(components) >= 4 && components[len(components)-3] == zfsSnapshotNamespace && components[len(components)-2] == zfsSnapshotImportNamespace {
		rootComponents = components[:len(components)-3]
	} else {
		switch components[len(components)-2] {
		case zfsBaseNamespace, zfsSandboxNamespace, zfsSnapshotNamespace:
		default:
			return "", false
		}
		rootComponents = components[:len(components)-2]
	}

	root := strings.TrimSpace(strings.Join(rootComponents, "/"))
	if root == "" {
		return "", false
	}
	return root, true
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

func datasetFromSnapshotRef(snapshotRef string) string {
	dataset, _, ok := strings.Cut(strings.TrimSpace(snapshotRef), "@")
	if !ok {
		return ""
	}
	return dataset
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

func parseZFSSendEstimateBytes(out []byte) int64 {
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "size" {
			continue
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || size < 0 {
			return 0
		}
		return size
	}
	return 0
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
