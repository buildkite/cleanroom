package backend

import (
	"context"

	"github.com/buildkite/cleanroom/internal/policy"
)

type Adapter interface {
	Name() string
	Run(ctx context.Context, req RunRequest) (*RunResult, error)
}

// PersistentSandboxAdapter supports provisioned sandbox instances that can run
// multiple executions before explicit termination.
type PersistentSandboxAdapter interface {
	Adapter
	ProvisionSandbox(ctx context.Context, req ProvisionRequest) error
	RunInSandbox(ctx context.Context, req RunRequest, stream OutputStream) (*RunResult, error)
	TerminateSandbox(ctx context.Context, sandboxID string) error
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

type AttachIO struct {
	WriteStdin func([]byte) error
	ResizeTTY  func(cols, rows uint32) error
}

type OutputStream struct {
	OnStdout func([]byte)
	OnStderr func([]byte)
	OnAttach func(AttachIO)
}

// StreamingAdapter can push stdout/stderr chunks while a command is running.
// Adapters that don't implement this continue to work via Run.
type StreamingAdapter interface {
	Adapter
	RunStream(ctx context.Context, req RunRequest, stream OutputStream) (*RunResult, error)
}

type RunRequest struct {
	SandboxID string
	RunID     string
	Command   []string
	TTY       bool
	Policy    *policy.CompiledPolicy
	FirecrackerConfig
}

type FirecrackerConfig struct {
	BinaryPath           string
	KernelImagePath      string
	RootFSPath           string
	PrivilegedMode       string
	PrivilegedHelperPath string
	RunDir               string
	VCPUs                int64
	MemoryMiB            int64
	GuestCID             uint32
	GuestPort            uint32
	Launch               bool
	LaunchSeconds        int64
}

type RunResult struct {
	RunID       string
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
