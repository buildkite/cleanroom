package backend

import (
	"context"

	"github.com/buildkite/cleanroom/internal/policy"
)

type Adapter interface {
	Name() string
	Run(ctx context.Context, req RunRequest) (*RunResult, error)
}

type RunRequest struct {
	RunID   string
	CWD     string
	Command []string
	Policy  *policy.CompiledPolicy
	FirecrackerConfig
}

type FirecrackerConfig struct {
	BinaryPath       string
	KernelImagePath  string
	RootFSPath       string
	RunDir           string
	WorkspaceHost    string
	WorkspaceMode    string // copy|mount
	WorkspacePersist string // discard|commit
	WorkspaceAccess  string // rw|ro
	VCPUs            int64
	MemoryMiB        int64
	GuestCID         uint32
	GuestPort        uint32
	RetainWrites     bool
	HostPassthrough  bool
	Launch           bool
	LaunchSeconds    int64
}

type RunResult struct {
	RunID      string
	ExitCode   int
	LaunchedVM bool
	PlanPath   string
	RunDir     string
	Message    string
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
