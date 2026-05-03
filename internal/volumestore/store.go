package volumestore

import (
	"context"
	"io"
)

type Driver interface {
	Name() string
	EnsureBaseVolume(context.Context, EnsureBaseVolumeRequest) (BaseVolume, error)
	CreateWritableVolume(context.Context, CreateWritableVolumeRequest) (WritableVolume, error)
	SnapshotVolume(context.Context, SnapshotVolumeRequest) (Snapshot, error)
	CloneSnapshotToVolume(context.Context, CloneSnapshotToVolumeRequest) (WritableVolume, error)
	DestroyVolume(context.Context, DestroyVolumeRequest) error
	DestroySnapshot(context.Context, DestroySnapshotRequest) error
}

// SnapshotDescriber reports driver-native metadata for an immutable snapshot.
type SnapshotDescriber interface {
	DescribeSnapshot(context.Context, DescribeSnapshotRequest) (SnapshotDescription, error)
}

// IncrementalSnapshotTransferDriver plans native incremental snapshot exports.
type IncrementalSnapshotTransferDriver interface {
	SnapshotDescriber
	PlanIncrementalSnapshotExport(context.Context, IncrementalSnapshotExportRequest) (IncrementalSnapshotExportPlan, error)
	ExportIncrementalSnapshot(context.Context, IncrementalSnapshotExportPlan, io.Writer) error
	ImportIncrementalSnapshot(context.Context, IncrementalSnapshotImportRequest, io.Reader) (Snapshot, error)
}

type EnsureBaseVolumeRequest struct {
	BaseID       string
	SourcePath   string
	MinimumBytes int64
}

type BaseVolume struct {
	Ref string
}

type CreateWritableVolumeRequest struct {
	VolumeID       string
	BaseRef        string
	AttachmentPath string
}

type CloneSnapshotToVolumeRequest struct {
	VolumeID       string
	SnapshotRef    string
	AttachmentPath string
}

type WritableVolume struct {
	Ref            string
	AttachmentPath string
}

type SnapshotVolumeRequest struct {
	SnapshotID string
	VolumeRef  string
}

type Snapshot struct {
	Ref                string
	StorageRef         string
	ParentSnapshotGUID string
	StorageSizeBytes   int64
	ExclusiveSizeBytes int64
	DriverMetadata     string
}

// DescribeSnapshotRequest identifies a snapshot by its driver-native ref.
type DescribeSnapshotRequest struct {
	SnapshotRef        string
	StorageRef         string
	ParentSnapshotGUID string
}

// SnapshotDescription is the driver-neutral metadata needed to validate a
// transfer candidate before importing it into local cache storage.
type SnapshotDescription struct {
	SnapshotRef        string
	StorageRef         string
	SnapshotGUID       string
	ParentSnapshotGUID string
	StorageSizeBytes   int64
	ExclusiveSizeBytes int64
}

// IncrementalSnapshotExportRequest asks a driver to validate an incremental
// transfer from one local snapshot to another.
type IncrementalSnapshotExportRequest struct {
	FromSnapshotRef  string
	FromSnapshotGUID string
	ToSnapshotRef    string
	ToSnapshotGUID   string
}

// IncrementalSnapshotExportPlan describes an incremental transfer that can be
// produced by the driver.
type IncrementalSnapshotExportPlan struct {
	FromSnapshotRef  string
	FromSnapshotGUID string
	ToSnapshotRef    string
	ToSnapshotGUID   string
	EstimatedBytes   int64
}

// IncrementalSnapshotImportRequest asks a driver to receive an incremental
// snapshot stream on top of a local parent snapshot.
type IncrementalSnapshotImportRequest struct {
	SnapshotID           string
	ParentSnapshotRef    string
	ParentSnapshotGUID   string
	ExpectedSnapshotGUID string
}

type DestroyVolumeRequest struct {
	VolumeRef string
}

type DestroySnapshotRequest struct {
	SnapshotRef string
}
