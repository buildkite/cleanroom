package backend

import (
	"context"
	"maps"
	"sort"

	"github.com/buildkite/cleanroom/internal/policy"
)

const (
	CapabilityExecStreaming          = "exec.streaming"
	CapabilitySandboxSnapshot        = "sandbox.snapshot"
	CapabilitySandboxPersistent      = "sandbox.persistent"
	CapabilitySandboxFileDownload    = "sandbox.file_download"
	CapabilityNetworkDefaultDeny     = "network.default_deny"
	CapabilityNetworkAllowlistEgress = "network.allowlist_egress"
	CapabilityNetworkGuestInterface  = "network.guest_interface"
)

var knownCapabilityKeys = []string{
	CapabilityExecStreaming,
	CapabilitySandboxSnapshot,
	CapabilitySandboxPersistent,
	CapabilitySandboxFileDownload,
	CapabilityNetworkDefaultDeny,
	CapabilityNetworkAllowlistEgress,
	CapabilityNetworkGuestInterface,
}

type Adapter interface {
	Name() string
	Run(ctx context.Context, req ExecutionRequest) (*ExecutionResult, error)
}

// CapabilityReporter allows backend adapters to publish backend-specific
// capability flags in a machine-readable form.
type CapabilityReporter interface {
	Capabilities() map[string]bool
}

// CapabilitiesForAdapter returns a merged capability map for the adapter.
//
// Baseline capabilities are inferred from backend interfaces:
// - StreamingAdapter => exec.streaming
// - PersistentSandboxAdapter => sandbox.persistent
// - SnapshottingAdapter => sandbox.snapshot
// - SandboxFileDownloadAdapter => sandbox.file_download
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
	if _, ok := adapter.(StreamingAdapter); ok {
		caps[CapabilityExecStreaming] = true
	}
	if _, ok := adapter.(PersistentSandboxAdapter); ok {
		caps[CapabilitySandboxPersistent] = true
	}
	if _, ok := adapter.(SnapshottingAdapter); ok {
		caps[CapabilitySandboxSnapshot] = true
	}
	if _, ok := adapter.(SandboxFileDownloadAdapter); ok {
		caps[CapabilitySandboxFileDownload] = true
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

// PersistentSandboxAdapter supports provisioned sandbox instances that can run
// multiple executions before explicit termination.
type PersistentSandboxAdapter interface {
	Adapter
	ProvisionSandbox(ctx context.Context, req ProvisionRequest) error
	RunInSandbox(ctx context.Context, req ExecutionRequest, stream OutputStream) (*ExecutionResult, error)
	TerminateSandbox(ctx context.Context, sandboxID string) error
}

// SnapshottingAdapter supports immutable filesystem snapshots of persistent
// sandboxes and creating new sandboxes from snapshots.
type SnapshottingAdapter interface {
	PersistentSandboxAdapter
	CreateSnapshot(ctx context.Context, req SnapshotRequest) (*SnapshotResult, error)
	ProvisionSandboxFromSnapshot(ctx context.Context, req ProvisionFromSnapshotRequest) error
	DeleteSnapshot(ctx context.Context, req DeleteSnapshotRequest) error
}

// SandboxFileDownloadAdapter can copy files out of a persistent sandbox.
type SandboxFileDownloadAdapter interface {
	DownloadSandboxFile(ctx context.Context, sandboxID, path string, maxBytes int64) ([]byte, error)
}

type ProvisionRequest struct {
	SandboxID string
	Policy    *policy.CompiledPolicy
	FirecrackerConfig
}

type SnapshotRequest struct {
	SandboxID  string
	SnapshotID string
	FirecrackerConfig
}

type SnapshotResult struct {
	StorageRef string
}

type ProvisionFromSnapshotRequest struct {
	SandboxID  string
	SnapshotID string
	StorageRef string
	Policy     *policy.CompiledPolicy
	FirecrackerConfig
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
}

type OutputStream struct {
	OnStdout  func([]byte)
	OnStderr  func([]byte)
	OnWarning func(string)
	OnAttach  func(AttachIO)
}

// StreamingAdapter can push stdout/stderr chunks while a command is running.
// Adapters that don't implement this continue to work via Run.
type StreamingAdapter interface {
	Adapter
	RunStream(ctx context.Context, req ExecutionRequest, stream OutputStream) (*ExecutionResult, error)
}

type ExecutionRequest struct {
	SandboxID   string
	ExecutionID string
	Command     []string
	TTY         bool
	Policy      *policy.CompiledPolicy
	FirecrackerConfig
}

type FirecrackerConfig struct {
	BinaryPath           string
	KernelImagePath      string
	RootFSPath           string
	MinimumRootFSBytes   int64
	DockerStartupSeconds int64
	DockerStorageDriver  string
	DockerIPTables       bool
	Snapshots            SnapshotConfig
	PrivilegedHelperPath string
	RunDir               string
	VCPUs                int64
	MemoryMiB            int64
	GuestCID             uint32
	GuestPort            uint32
	Launch               bool
	LaunchSeconds        int64
}

type SnapshotConfig struct {
	Enabled               bool
	Driver                string
	BaseDir               string
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
	Stdout      string
	Stderr      string
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
