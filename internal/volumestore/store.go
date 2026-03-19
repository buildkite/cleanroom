package volumestore

import "context"

type Driver interface {
	Name() string
	EnsureBaseVolume(context.Context, EnsureBaseVolumeRequest) (BaseVolume, error)
	CreateWritableVolume(context.Context, CreateWritableVolumeRequest) (WritableVolume, error)
	SnapshotVolume(context.Context, SnapshotVolumeRequest) (Snapshot, error)
	CloneSnapshotToVolume(context.Context, CloneSnapshotToVolumeRequest) (WritableVolume, error)
	DestroyVolume(context.Context, DestroyVolumeRequest) error
	DestroySnapshot(context.Context, DestroySnapshotRequest) error
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
	Ref        string
	StorageRef string
}

type DestroyVolumeRequest struct {
	VolumeRef string
}

type DestroySnapshotRequest struct {
	SnapshotRef string
}
