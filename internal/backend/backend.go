package backend

import (
	"context"

	"github.com/buildkite/cleanroom/internal/policy"
)

type Adapter interface {
	Name() string
	Run(ctx context.Context, req RunRequest) (*RunResult, error)
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
	RunID   string
	CWD     string
	Command []string
	TTY     bool
	Policy  *policy.CompiledPolicy
	FirecrackerConfig
}

type FirecrackerConfig struct {
	BinaryPath      string
	KernelImagePath string
	RootFSPath      string
	RunDir          string
	WorkspaceHost   string
	WorkspaceAccess string // rw|ro
	VCPUs           int64
	MemoryMiB       int64
	GuestCID        uint32
	GuestPort       uint32
	HostPassthrough bool
	Launch          bool
	LaunchSeconds   int64
}

type RunResult struct {
	RunID      string
	ExitCode   int
	LaunchedVM bool
	PlanPath   string
	RunDir     string
	Message    string
	Stdout     string
	Stderr     string
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
