package controlservice

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
}

type loader interface {
	LoadAndCompile(cwd string) (*policy.CompiledPolicy, string, error)
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
		BinaryPath:       cfg.Backends.Firecracker.BinaryPath,
		KernelImagePath:  cfg.Backends.Firecracker.KernelImage,
		RootFSPath:       cfg.Backends.Firecracker.RootFS,
		WorkspaceHost:    cwd,
		WorkspaceMode:    resolveWorkspaceMode(cfg.Workspace.Mode),
		WorkspacePersist: resolveWorkspacePersist(cfg.Workspace.Persist),
		WorkspaceAccess:  resolveWorkspaceAccess(opts, cfg.Workspace.Access),
		VCPUs:            cfg.Backends.Firecracker.VCPUs,
		MemoryMiB:        cfg.Backends.Firecracker.MemoryMiB,
		GuestCID:         cfg.Backends.Firecracker.GuestCID,
		GuestPort:        cfg.Backends.Firecracker.GuestPort,
		RetainWrites:     cfg.Backends.Firecracker.RetainWrites,
		LaunchSeconds:    cfg.Backends.Firecracker.LaunchSeconds,
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

func resolveWorkspaceMode(configured string) string {
	mode := configured
	if mode == "" {
		mode = "copy"
	}
	return mode
}

func resolveWorkspacePersist(configured string) string {
	persist := configured
	if persist == "" {
		persist = "discard"
	}
	return persist
}
