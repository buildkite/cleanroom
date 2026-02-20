package controlservice

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlapi"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/charmbracelet/log"
)

type Service struct {
	Loader   loader
	Config   runtimeconfig.Config
	Backends map[string]backend.Adapter
	Logger   *log.Logger

	mu         sync.RWMutex
	cleanrooms map[string]launchedCleanroom
}

type launchedCleanroom struct {
	ID               string
	CWD              string
	Backend          string
	Policy           *policy.CompiledPolicy
	PolicySource     string
	Firecracker      backend.FirecrackerConfig
	RunDirRoot       string
	CreatedAt        time.Time
	LastExecutionRun string
}

type loader interface {
	LoadAndCompile(cwd string) (*policy.CompiledPolicy, string, error)
}

func (s *Service) LaunchCleanroom(_ context.Context, req controlapi.LaunchCleanroomRequest) (*controlapi.LaunchCleanroomResponse, error) {
	if strings.TrimSpace(req.CWD) == "" {
		return nil, errors.New("missing cwd")
	}

	backendName := resolveBackendName(req.Backend, s.Config.DefaultBackend)
	if _, ok := s.Backends[backendName]; !ok {
		return nil, fmt.Errorf("unknown backend %q", backendName)
	}

	compiled, source, err := s.Loader.LoadAndCompile(req.CWD)
	if err != nil {
		return nil, err
	}

	cleanroomID := fmt.Sprintf("cr-%d", time.Now().UTC().UnixNano())
	launchOpts := controlapi.ExecOptions{
		RunDir:            req.Options.RunDir,
		ReadOnlyWorkspace: req.Options.ReadOnlyWorkspace,
		LaunchSeconds:     req.Options.LaunchSeconds,
	}
	firecrackerCfg := mergeFirecrackerConfig(req.CWD, launchOpts, s.Config)
	runDirRoot := strings.TrimSpace(req.Options.RunDir)
	firecrackerCfg.RunDir = ""

	state := launchedCleanroom{
		ID:           cleanroomID,
		CWD:          req.CWD,
		Backend:      backendName,
		Policy:       compiled,
		PolicySource: source,
		Firecracker:  firecrackerCfg,
		RunDirRoot:   runDirRoot,
		CreatedAt:    time.Now().UTC(),
	}

	s.mu.Lock()
	if s.cleanrooms == nil {
		s.cleanrooms = map[string]launchedCleanroom{}
	}
	s.cleanrooms[cleanroomID] = state
	s.mu.Unlock()

	if s.Logger != nil {
		s.Logger.Info("cleanroom launched",
			"cleanroom_id", cleanroomID,
			"backend", backendName,
			"cwd", req.CWD,
			"policy_source", source,
			"policy_hash", compiled.Hash,
		)
	}

	return &controlapi.LaunchCleanroomResponse{
		CleanroomID:  cleanroomID,
		Backend:      backendName,
		PolicySource: source,
		PolicyHash:   compiled.Hash,
		RunDirRoot:   runDirRoot,
		Message:      "cleanroom launched and ready to run commands",
	}, nil
}

func (s *Service) RunCleanroom(ctx context.Context, req controlapi.RunCleanroomRequest) (*controlapi.RunCleanroomResponse, error) {
	cleanroomID := strings.TrimSpace(req.CleanroomID)
	if cleanroomID == "" {
		return nil, errors.New("missing cleanroom_id")
	}

	command := normalizeCommand(req.Command)
	if len(command) == 0 {
		return nil, errors.New("missing command")
	}

	s.mu.RLock()
	state, ok := s.cleanrooms[cleanroomID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown cleanroom %q", cleanroomID)
	}

	adapter, ok := s.Backends[state.Backend]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q", state.Backend)
	}

	runID := fmt.Sprintf("run-%d", time.Now().UTC().UnixNano())
	firecrackerCfg := state.Firecracker
	if state.RunDirRoot != "" {
		firecrackerCfg.RunDir = filepath.Join(state.RunDirRoot, runID)
	}

	result, err := adapter.Run(ctx, backend.RunRequest{
		RunID:             runID,
		CWD:               state.CWD,
		Command:           append([]string(nil), command...),
		Policy:            state.Policy,
		FirecrackerConfig: firecrackerCfg,
	})
	if err != nil {
		if s.Logger != nil {
			s.Logger.Error("cleanroom command failed",
				"cleanroom_id", cleanroomID,
				"backend", state.Backend,
				"error", err,
			)
		}
		return nil, err
	}

	s.mu.Lock()
	if current, found := s.cleanrooms[cleanroomID]; found {
		current.LastExecutionRun = result.RunID
		s.cleanrooms[cleanroomID] = current
	}
	s.mu.Unlock()

	if s.Logger != nil {
		s.Logger.Info("cleanroom command completed",
			"cleanroom_id", cleanroomID,
			"run_id", result.RunID,
			"exit_code", result.ExitCode,
			"launched_vm", result.LaunchedVM,
		)
	}

	return &controlapi.RunCleanroomResponse{
		CleanroomID: cleanroomID,
		RunID:       result.RunID,
		ExitCode:    result.ExitCode,
		LaunchedVM:  result.LaunchedVM,
		PlanPath:    result.PlanPath,
		RunDir:      result.RunDir,
		Message:     result.Message,
		Stdout:      result.Stdout,
		Stderr:      result.Stderr,
	}, nil
}

func (s *Service) TerminateCleanroom(_ context.Context, req controlapi.TerminateCleanroomRequest) (*controlapi.TerminateCleanroomResponse, error) {
	cleanroomID := strings.TrimSpace(req.CleanroomID)
	if cleanroomID == "" {
		return nil, errors.New("missing cleanroom_id")
	}

	s.mu.Lock()
	state, ok := s.cleanrooms[cleanroomID]
	if ok {
		delete(s.cleanrooms, cleanroomID)
	}
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown cleanroom %q", cleanroomID)
	}

	if s.Logger != nil {
		s.Logger.Info("cleanroom terminated",
			"cleanroom_id", cleanroomID,
			"backend", state.Backend,
			"last_run_id", state.LastExecutionRun,
		)
	}

	return &controlapi.TerminateCleanroomResponse{
		CleanroomID: cleanroomID,
		Terminated:  true,
		Message:     "cleanroom terminated",
	}, nil
}

func (s *Service) Exec(ctx context.Context, req controlapi.ExecRequest) (*controlapi.ExecResponse, error) {
	started := time.Now()
	command := normalizeCommand(req.Command)
	if len(command) == 0 {
		return nil, errors.New("missing command")
	}
	if strings.TrimSpace(req.CWD) == "" {
		return nil, errors.New("missing cwd")
	}

	backendName := resolveBackendName(req.Backend, s.Config.DefaultBackend)
	adapter, ok := s.Backends[backendName]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q", backendName)
	}
	if s.Logger != nil {
		s.Logger.Debug("execution request received",
			"cwd", req.CWD,
			"backend", backendName,
			"command_argc", len(command),
			"dry_run", req.Options.DryRun,
			"host_passthrough", req.Options.HostPassthrough,
		)
	}

	compiled, source, err := s.Loader.LoadAndCompile(req.CWD)
	if err != nil {
		return nil, err
	}

	runID := fmt.Sprintf("run-%d", time.Now().UTC().UnixNano())
	runReq := backend.RunRequest{
		RunID:             runID,
		CWD:               req.CWD,
		Command:           append([]string(nil), command...),
		Policy:            compiled,
		FirecrackerConfig: mergeFirecrackerConfig(req.CWD, req.Options, s.Config),
	}

	result, err := adapter.Run(ctx, runReq)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Error("execution failed", "backend", backendName, "error", err)
		}
		return nil, err
	}
	if s.Logger != nil {
		s.Logger.Info("execution completed",
			"backend", backendName,
			"run_id", result.RunID,
			"policy_source", source,
			"policy_hash", compiled.Hash,
			"plan", result.PlanPath,
			"run_dir", result.RunDir,
			"exit_code", result.ExitCode,
			"launched_vm", result.LaunchedVM,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	}

	return &controlapi.ExecResponse{
		RunID:        result.RunID,
		PolicySource: source,
		PolicyHash:   compiled.Hash,
		ExitCode:     result.ExitCode,
		LaunchedVM:   result.LaunchedVM,
		PlanPath:     result.PlanPath,
		RunDir:       result.RunDir,
		Message:      result.Message,
		Stdout:       result.Stdout,
		Stderr:       result.Stderr,
	}, nil
}

func normalizeCommand(command []string) []string {
	if len(command) > 0 && command[0] == "--" {
		return command[1:]
	}
	return command
}

func resolveBackendName(requested, configuredDefault string) string {
	if requested != "" {
		return requested
	}
	if configuredDefault != "" {
		return configuredDefault
	}
	return "firecracker"
}

func mergeFirecrackerConfig(cwd string, opts controlapi.ExecOptions, cfg runtimeconfig.Config) backend.FirecrackerConfig {
	out := backend.FirecrackerConfig{
		BinaryPath:      cfg.Backends.Firecracker.BinaryPath,
		KernelImagePath: cfg.Backends.Firecracker.KernelImage,
		RootFSPath:      cfg.Backends.Firecracker.RootFS,
		WorkspaceHost:   cwd,
		WorkspaceAccess: resolveWorkspaceAccess(opts, cfg.Workspace.Access),
		VCPUs:           cfg.Backends.Firecracker.VCPUs,
		MemoryMiB:       cfg.Backends.Firecracker.MemoryMiB,
		GuestCID:        cfg.Backends.Firecracker.GuestCID,
		GuestPort:       cfg.Backends.Firecracker.GuestPort,
		LaunchSeconds:   cfg.Backends.Firecracker.LaunchSeconds,
	}

	if opts.RunDir != "" {
		out.RunDir = opts.RunDir
	}
	out.Launch = true
	if opts.DryRun || opts.HostPassthrough {
		out.Launch = false
	}
	out.HostPassthrough = opts.HostPassthrough
	if opts.LaunchSeconds != 0 {
		out.LaunchSeconds = opts.LaunchSeconds
	}
	return out
}

func resolveWorkspaceAccess(execCfg controlapi.ExecOptions, configured string) string {
	access := configured
	if access == "" {
		access = "rw"
	}
	if execCfg.ReadOnlyWorkspace {
		access = "ro"
	}
	return access
}
