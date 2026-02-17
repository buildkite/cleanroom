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
	BinaryPath      string
	KernelImagePath string
	RootFSPath      string
	RunDir          string
	VCPUs           int64
	MemoryMiB       int64
	RetainWrites    bool
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
}
