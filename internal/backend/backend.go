package backend

import (
	"context"
	"io"
	"io/fs"
	"maps"
	"net"
	"sort"
	"time"

	"github.com/buildkite/cleanroom/internal/policy"
)

const (
	CapabilityExecStreaming               = "exec.streaming"
	CapabilitySandboxSnapshot             = "sandbox.snapshot"
	CapabilitySandboxFileDownload         = "sandbox.file_download"
	CapabilitySandboxFileUpload           = "sandbox.file_upload"
	CapabilitySandboxPathStat             = "sandbox.path_stat"
	CapabilitySandboxTreeWalk             = "sandbox.tree_walk"
	CapabilitySandboxFileRead             = "sandbox.file_read"
	CapabilitySandboxFileWrite            = "sandbox.file_write"
	CapabilitySandboxPathRemove           = "sandbox.path_remove"
	CapabilitySandboxArchiveRead          = "sandbox.archive_read"
	CapabilitySandboxArchiveWrite         = "sandbox.archive_write"
	CapabilityNetworkDefaultDeny          = "network.default_deny"
	CapabilityNetworkAllowlistEgress      = "network.allowlist_egress"
	CapabilityNetworkStageScopedEgress    = "network.stage_scoped_egress"
	CapabilityDNSControlOrEquivalent      = "dns_control_or_equivalent"
	CapabilityNetworkGuestInterface       = "network.guest_interface"
	CapabilitySandboxPortDial             = "sandbox.port_dial"
	CapabilitySandboxCacheOutputVolumes   = "sandbox.cache_output_volumes"
	CapabilitySandboxCacheOutputFastClone = "sandbox.cache_output_fast_clone"
	CapabilitySandboxOverlayWriteCapture  = "sandbox.overlay_write_capture"
)

var knownCapabilityKeys = []string{
	CapabilityExecStreaming,
	CapabilitySandboxSnapshot,
	CapabilitySandboxFileDownload,
	CapabilitySandboxFileUpload,
	CapabilitySandboxPathStat,
	CapabilitySandboxTreeWalk,
	CapabilitySandboxFileRead,
	CapabilitySandboxFileWrite,
	CapabilitySandboxPathRemove,
	CapabilitySandboxArchiveRead,
	CapabilitySandboxArchiveWrite,
	CapabilityNetworkDefaultDeny,
	CapabilityNetworkAllowlistEgress,
	CapabilityNetworkStageScopedEgress,
	CapabilityDNSControlOrEquivalent,
	CapabilityNetworkGuestInterface,
	CapabilitySandboxPortDial,
	CapabilitySandboxCacheOutputVolumes,
	CapabilitySandboxCacheOutputFastClone,
	CapabilitySandboxOverlayWriteCapture,
}

type Adapter interface {
	Name() string
	ProvisionSandbox(ctx context.Context, req ProvisionRequest) error
	RunInSandbox(ctx context.Context, req ExecutionRequest, stream OutputStream) (*ExecutionResult, error)
	TerminateSandbox(ctx context.Context, sandboxID string) error
}

// CapabilityReporter allows backend adapters to publish backend-specific
// capability flags in a machine-readable form.
type CapabilityReporter interface {
	Capabilities() map[string]bool
}

// CapabilitiesForAdapter returns a merged capability map for the adapter.
//
// Baseline capabilities are inferred from backend interfaces:
// - Adapter => exec.streaming
// - SnapshottingAdapter => sandbox.snapshot
// - SandboxFileDownloadAdapter => sandbox.file_download
// - SandboxFileUploadAdapter => sandbox.file_upload
// - SandboxPathStatAdapter => sandbox.path_stat
// - SandboxTreeWalkAdapter => sandbox.tree_walk
// - SandboxFileReadAdapter => sandbox.file_read
// - SandboxFileWriteAdapter => sandbox.file_write
// - SandboxPathRemoveAdapter => sandbox.path_remove
// - SandboxArchiveReadAdapter => sandbox.archive_read
// - SandboxArchiveWriteAdapter => sandbox.archive_write
// - SandboxPortDialer => sandbox.port_dial
//
// Additional backend-specific capabilities can be provided by implementing
// CapabilityReporter.
func CapabilitiesForAdapter(adapter Adapter) map[string]bool {
	caps := make(map[string]bool, len(knownCapabilityKeys))
	for _, key := range knownCapabilityKeys {
		caps[key] = false
	}

	if adapter == nil {
		return caps
	}
	caps[CapabilityExecStreaming] = true
	if _, ok := adapter.(SnapshottingAdapter); ok {
		caps[CapabilitySandboxSnapshot] = true
	}
	if _, ok := adapter.(SandboxFileDownloadAdapter); ok {
		caps[CapabilitySandboxFileDownload] = true
	}
	if _, ok := adapter.(SandboxFileUploadAdapter); ok {
		caps[CapabilitySandboxFileUpload] = true
	}
	if _, ok := adapter.(SandboxPathStatAdapter); ok {
		caps[CapabilitySandboxPathStat] = true
	}
	if _, ok := adapter.(SandboxTreeWalkAdapter); ok {
		caps[CapabilitySandboxTreeWalk] = true
	}
	if _, ok := adapter.(SandboxFileReadAdapter); ok {
		caps[CapabilitySandboxFileRead] = true
	}
	if _, ok := adapter.(SandboxFileWriteAdapter); ok {
		caps[CapabilitySandboxFileWrite] = true
	}
	if _, ok := adapter.(SandboxPathRemoveAdapter); ok {
		caps[CapabilitySandboxPathRemove] = true
	}
	if _, ok := adapter.(SandboxArchiveReadAdapter); ok {
		caps[CapabilitySandboxArchiveRead] = true
	}
	if _, ok := adapter.(SandboxArchiveWriteAdapter); ok {
		caps[CapabilitySandboxArchiveWrite] = true
	}
	if _, ok := adapter.(SandboxPortDialer); ok {
		caps[CapabilitySandboxPortDial] = true
	}

	if reporter, ok := adapter.(CapabilityReporter); ok {
		for key, value := range reporter.Capabilities() {
			caps[key] = value
		}
	}

	return caps
}

// SortedCapabilityKeys returns deterministic capability keys for presentation.
func SortedCapabilityKeys(caps map[string]bool) []string {
	keys := make([]string, 0, len(caps))
	for key := range caps {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// CloneCapabilities returns a detached copy of the capability map.
func CloneCapabilities(caps map[string]bool) map[string]bool {
	out := make(map[string]bool, len(caps))
	maps.Copy(out, caps)
	return out
}

// Adapter supports provisioned sandbox instances that can run multiple
// executions before explicit termination.

// SnapshottingAdapter supports immutable filesystem snapshots of persistent
// sandboxes and creating new sandboxes from snapshots.
type SnapshottingAdapter interface {
	Adapter
	CreateSnapshot(ctx context.Context, req SnapshotRequest) (*SnapshotResult, error)
	ProvisionSandboxFromSnapshot(ctx context.Context, req ProvisionFromSnapshotRequest) error
	DeleteSnapshot(ctx context.Context, req DeleteSnapshotRequest) error
}

// CacheOutputVolumeSnapshotter snapshots cache output volumes that were
// attached to an already-provisioned sandbox.
type CacheOutputVolumeSnapshotter interface {
	Adapter
	SnapshotCacheOutputVolumes(ctx context.Context, req SnapshotCacheOutputVolumesRequest) (*SnapshotCacheOutputVolumesResult, error)
}

// RuntimeBaseKeyProvider returns a stable identifier for the backend runtime
// base that a reusable workspace stage depends on.
type RuntimeBaseKeyProvider interface {
	RuntimeBaseKey(ctx context.Context, compiled *policy.CompiledPolicy, cfg FirecrackerConfig) (string, error)
}

type SandboxPathType string

const (
	SandboxPathTypeFile      SandboxPathType = "file"
	SandboxPathTypeDirectory SandboxPathType = "directory"
	SandboxPathTypeSymlink   SandboxPathType = "symlink"
	SandboxPathTypeOther     SandboxPathType = "other"
)

type SandboxPathInfo struct {
	Path          string
	Type          SandboxPathType
	SizeBytes     int64
	Mode          fs.FileMode
	MTime         time.Time
	SymlinkTarget string
}

// SandboxFileDownloadAdapter can copy files out of a persistent sandbox.
type SandboxFileDownloadAdapter interface {
	DownloadSandboxFile(ctx context.Context, sandboxID, path string, maxBytes int64) ([]byte, error)
}

// SandboxFileUploadAdapter can copy files into a persistent sandbox.
type SandboxFileUploadAdapter interface {
	UploadSandboxFile(ctx context.Context, sandboxID, path string, data []byte, mode fs.FileMode) error
}

type SandboxPathStatAdapter interface {
	StatSandboxPath(ctx context.Context, sandboxID, path string) (*SandboxPathInfo, error)
}

type SandboxTreeWalkAdapter interface {
	WalkSandboxTree(ctx context.Context, sandboxID, path string, emit func(SandboxPathInfo) error) error
}

type SandboxFileReadAdapter interface {
	ReadSandboxFile(ctx context.Context, sandboxID, path string, maxBytes int64, emit func([]byte) error) error
}

type SandboxFileWriteAdapter interface {
	WriteSandboxFile(ctx context.Context, sandboxID, path string, r io.Reader, mode fs.FileMode, mtime time.Time) (int64, error)
}

type SandboxPathRemoveAdapter interface {
	RemoveSandboxPath(ctx context.Context, sandboxID, path string, recursive bool) error
}

type SandboxArchiveReadAdapter interface {
	ArchiveSandboxPaths(ctx context.Context, sandboxID string, paths []string, maxBytes int64, emit func([]byte) error) error
}

type SandboxArchiveWriteAdapter interface {
	ExtractSandboxArchive(ctx context.Context, sandboxID, destination string, r io.Reader) (int64, error)
}

type SandboxPortDialer interface {
	DialSandboxPort(ctx context.Context, sandboxID string, port int) (net.Conn, error)
}

type ProvisionRequest struct {
	SandboxID          string
	Policy             *policy.CompiledPolicy
	CacheOutputVolumes []CacheOutputVolumeSpec
	FirecrackerConfig
}

type SnapshotRequest struct {
	SandboxID  string
	SnapshotID string
	FirecrackerConfig
}

type SnapshotResult struct {
	StorageRef         string
	StorageSizeBytes   int64
	ExclusiveSizeBytes int64
	DriverMetadata     string
}

type SnapshotCacheOutputVolumesRequest struct {
	SandboxID string
	Volumes   []CacheOutputVolumeSnapshotRequest
	FirecrackerConfig
}

type CacheOutputVolumeSnapshotRequest struct {
	Stage      string
	BlockName  string
	CacheKey   string
	VolumeID   string
	SnapshotID string
}

type SnapshotCacheOutputVolumesResult struct {
	Volumes []CacheOutputVolumeSnapshot
}

type CacheOutputVolumeSnapshot struct {
	Stage              string
	BlockName          string
	CacheKey           string
	VolumeID           string
	SnapshotID         string
	StorageDriver      string
	StorageRef         string
	StorageSizeBytes   int64
	ExclusiveSizeBytes int64
	DriverMetadata     string
}

type ProvisionFromSnapshotRequest struct {
	SandboxID          string
	SnapshotID         string
	StorageRef         string
	Policy             *policy.CompiledPolicy
	CacheOutputVolumes []CacheOutputVolumeSpec
	FirecrackerConfig
}

type CacheOutputVolumeSpec struct {
	Stage             string
	BlockName         string
	CacheKey          string
	VolumeID          string
	SourceSnapshotRef string
	StorageDriver     string
	StorageRef        string
	DirMappings       []CacheOutputDirMapping
	FileMappings      []CacheOutputFileMapping
}

type CacheOutputDirMapping struct {
	GuestPath string
	Subpath   string
}

type CacheOutputFileMapping struct {
	GuestPath string
	Subpath   string
	Mode      fs.FileMode
}

type DeleteSnapshotRequest struct {
	SnapshotID string
	StorageRef string
	FirecrackerConfig
}

type AttachIO struct {
	WriteStdin func([]byte) error
	CloseStdin func() error
	ResizeTTY  func(cols, rows uint32) error
	Metadata   map[string]string
}

type OutputStream struct {
	OnStdout  func([]byte)
	OnStderr  func([]byte)
	OnWarning func(string)
	OnAttach  func(AttachIO)
}

type ExecutionRequest struct {
	SandboxID       string
	ExecutionID     string
	Command         []string
	Dir             string
	Env             []string
	ClosedEnv       bool
	TTY             bool
	InputProjection *InputProjection
	Policy          *policy.CompiledPolicy
	NetworkStage    policy.NetworkStage
	FirecrackerConfig
}

type InputProjection struct {
	SourceRoot          string
	TargetRoot          string
	Files               []string
	MountSourceReadOnly bool
}

type FirecrackerConfig struct {
	BinaryPath                                string
	KernelImagePath                           string
	RootFSPath                                string
	MinimumRootFSBytes                        int64
	DarwinVZNetworkMode                       string
	DarwinVZNetworkSubnet                     string
	DarwinVZNetworkExternalInterface          string
	DarwinVZNetworkDisableNAT44               bool
	DarwinVZNetworkDisableNAT66               bool
	DarwinVZNetworkDisableDNSProxy            bool
	DarwinVZNetworkDisableRouterAdvertisement bool
	DockerStartupSeconds                      int64
	DockerStorageDriver                       string
	DockerIPTables                            bool
	Snapshots                                 SnapshotConfig
	PrivilegedMode                            string
	PrivilegedHelperPath                      string
	RunDir                                    string
	VCPUs                                     int64
	MemoryMiB                                 int64
	GuestCID                                  uint32
	GuestPort                                 uint32
	Launch                                    bool
	LaunchSeconds                             int64
}

type SnapshotConfig struct {
	Enabled               bool
	Driver                string
	BaseDir               string
	ZFSDataset            string
	QuiesceTimeoutSeconds int64
}

type ExecutionResult struct {
	ExecutionID string
	ExitCode    int
	LaunchedVM  bool
	PlanPath    string
	RunDir      string
	ImageRef    string
	ImageDigest string
	Message     string
}

type DoctorRequest struct {
	Policy *policy.CompiledPolicy
	FirecrackerConfig
}

type DoctorReport struct {
	Backend string        `json:"backend"`
	Checks  []DoctorCheck `json:"checks"`
}

type DoctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // pass|warn|fail
	Message string `json:"message"`
}
