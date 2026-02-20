package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/backend/firecracker"
	"github.com/buildkite/cleanroom/internal/controlapi"
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/charmbracelet/log"
)

type runtimeContext struct {
	CWD        string
	Stdout     *os.File
	Loader     policy.Loader
	Config     runtimeconfig.Config
	ConfigPath string
	Backends   map[string]backend.Adapter
}

type CLI struct {
	Policy PolicyCommand `cmd:"" help:"Policy commands"`
	Exec   ExecCommand   `cmd:"" help:"Execute a command in a cleanroom backend"`
	Serve  ServeCommand  `cmd:"" help:"Run the cleanroom control-plane server"`
	Doctor DoctorCommand `cmd:"" help:"Run environment and backend diagnostics"`
	Status StatusCommand `cmd:"" help:"Inspect run artifacts"`
}

type PolicyCommand struct {
	Validate PolicyValidateCommand `cmd:"" help:"Validate policy configuration"`
}

type PolicyValidateCommand struct {
	Chdir string `short:"c" help:"Change to this directory before running commands"`
	JSON  bool   `help:"Print compiled policy as JSON"`
}

type ExecCommand struct {
	Chdir    string `short:"c" help:"Change to this directory before running commands"`
	Host     string `help:"Control-plane endpoint (unix://path, http://host:port, or https://host:port)"`
	LogLevel string `help:"Client log level (debug|info|warn|error)"`
	Backend  string `help:"Execution backend (defaults to runtime config or firecracker)"`

	RunDir            string `help:"Run directory for generated artifacts (default: XDG runtime/state cleanroom path)"`
	ReadOnlyWorkspace bool   `help:"Mount workspace read-only for this run"`
	DryRun            bool   `help:"Generate execution plan without running a backend command"`
	HostPassthrough   bool   `help:"Run command directly on host instead of launching a backend (unsafe, not sandboxed)"`
	LaunchSeconds     int64  `help:"VM boot/guest-agent readiness timeout in seconds"`

	Command []string `arg:"" passthrough:"" required:"" help:"Command to execute"`
}

type ServeCommand struct {
	Listen   string `help:"Listen endpoint for control API (defaults to runtime endpoint)"`
	LogLevel string `help:"Server log level (debug|info|warn|error)"`
}

type StatusCommand struct {
	RunID   string `help:"Run ID to inspect"`
	LastRun bool   `help:"Inspect the most recent run"`
}

type DoctorCommand struct {
	Chdir   string `short:"c" help:"Change to this directory before running commands"`
	Backend string `help:"Execution backend to diagnose (defaults to runtime config or firecracker)"`
	JSON    bool   `help:"Print doctor report as JSON"`
}

type exitCodeError struct {
	code int
}

func (e exitCodeError) Error() string {
	return fmt.Sprintf("command failed with exit code %d", e.code)
}

func (e exitCodeError) ExitCode() int {
	return e.code
}

type hasExitCode interface {
	ExitCode() int
}

func Run(args []string) error {
	cfg, cfgPath, err := runtimeconfig.Load()
	if err != nil {
		return err
	}

	runtimeCtx := &runtimeContext{
		Stdout:     os.Stdout,
		Loader:     policy.Loader{},
		Config:     cfg,
		ConfigPath: cfgPath,
		Backends: map[string]backend.Adapter{
			"firecracker": firecracker.New(),
		},
	}

	cli := CLI{}
	parser, err := kong.New(
		&cli,
		kong.Name("cleanroom"),
		kong.Description("Cleanroom CLI (MVP)"),
	)
	if err != nil {
		return err
	}

	ctx, err := parser.Parse(args)
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	runtimeCtx.CWD = cwd

	return ctx.Run(runtimeCtx)
}

func ExitCode(err error) int {
	var codeErr hasExitCode
	if errors.As(err, &codeErr) {
		return codeErr.ExitCode()
	}
	return 1
}

func (c *PolicyValidateCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, c.Chdir)
	if err != nil {
		return err
	}
	compiled, source, err := ctx.Loader.LoadAndCompile(cwd)
	if err != nil {
		return err
	}

	if c.JSON {
		payload := map[string]any{
			"source": source,
			"policy": compiled,
		}
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	_, err = fmt.Fprintf(ctx.Stdout, "policy valid: %s\npolicy hash: %s\n", source, compiled.Hash)
	return err
}

func (e *ExecCommand) Run(ctx *runtimeContext) error {
	logger, err := newLogger(e.LogLevel, "client")
	if err != nil {
		return err
	}

	cwd, err := resolveCWD(ctx.CWD, e.Chdir)
	if err != nil {
		return err
	}
	ep, err := endpoint.Resolve(e.Host)
	if err != nil {
		return err
	}
	logger.Debug("sending execution request",
		"endpoint", ep.Address,
		"cwd", cwd,
		"backend", e.Backend,
		"command_argc", len(e.Command),
		"dry_run", e.DryRun,
		"host_passthrough", e.HostPassthrough,
	)
	client := controlclient.New(ep)
	resp, err := client.Exec(context.Background(), controlapi.ExecRequest{
		CWD:     cwd,
		Backend: e.Backend,
		Command: append([]string(nil), e.Command...),
		Options: controlapi.ExecOptions{
			RunDir:            e.RunDir,
			ReadOnlyWorkspace: e.ReadOnlyWorkspace,
			DryRun:            e.DryRun,
			HostPassthrough:   e.HostPassthrough,
			LaunchSeconds:     e.LaunchSeconds,
		},
	})
	if err != nil {
		return fmt.Errorf("execute via control-plane endpoint %q: %w", ep.Address, err)
	}
	logger.Debug("execution complete",
		"run_id", resp.RunID,
		"policy_source", resp.PolicySource,
		"policy_hash", resp.PolicyHash,
		"plan", resp.PlanPath,
		"run_dir", resp.RunDir,
		"launched_vm", resp.LaunchedVM,
		"exit_code", resp.ExitCode,
	)

	if resp.Stdout != "" {
		if _, err := fmt.Fprint(ctx.Stdout, resp.Stdout); err != nil {
			return err
		}
	}
	if resp.Stderr != "" {
		if _, err := fmt.Fprint(os.Stderr, resp.Stderr); err != nil {
			return err
		}
	}

	if resp.ExitCode != 0 {
		return exitCodeError{code: resp.ExitCode}
	}
	return nil
}

func (s *ServeCommand) Run(ctx *runtimeContext) error {
	logger, err := newLogger(s.LogLevel, "server")
	if err != nil {
		return err
	}

	ep, err := endpoint.Resolve(s.Listen)
	if err != nil {
		return err
	}

	service := &controlservice.Service{
		Loader:   ctx.Loader,
		Config:   ctx.Config,
		Backends: ctx.Backends,
		Logger:   logger.With("subsystem", "service"),
	}
	server := controlserver.New(service, logger.With("subsystem", "http"))

	runCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return controlserver.Serve(runCtx, ep, server.Handler(), logger)
}

func (d *DoctorCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, d.Chdir)
	if err != nil {
		return err
	}
	backendName := resolveBackendName(d.Backend, ctx.Config.DefaultBackend)
	adapter, ok := ctx.Backends[backendName]
	if !ok {
		return fmt.Errorf("unknown backend %q", backendName)
	}

	checks := []backend.DoctorCheck{
		{Name: "runtime_config", Status: "pass", Message: fmt.Sprintf("using runtime config path %s", ctx.ConfigPath)},
		{Name: "backend", Status: "pass", Message: fmt.Sprintf("selected backend %s", backendName)},
	}

	compiled, source, err := ctx.Loader.LoadAndCompile(cwd)
	if err != nil {
		checks = append(checks, backend.DoctorCheck{
			Name:    "repository_policy",
			Status:  "warn",
			Message: fmt.Sprintf("policy not loaded from %s: %v", cwd, err),
		})
	} else {
		checks = append(checks, backend.DoctorCheck{
			Name:    "repository_policy",
			Status:  "pass",
			Message: fmt.Sprintf("policy loaded from %s (hash %s)", source, compiled.Hash),
		})
	}

	type doctorCapable interface {
		Doctor(context.Context, backend.DoctorRequest) (*backend.DoctorReport, error)
	}
	if checker, ok := adapter.(doctorCapable); ok {
		report, err := checker.Doctor(context.Background(), backend.DoctorRequest{
			Policy:            compiled,
			FirecrackerConfig: mergeFirecrackerConfig(ctx.CWD, &ExecCommand{}, ctx.Config),
		})
		if err != nil {
			return err
		}
		checks = append(checks, report.Checks...)
	} else {
		checks = append(checks, backend.DoctorCheck{
			Name:    "backend_doctor",
			Status:  "warn",
			Message: "selected backend does not expose doctor diagnostics",
		})
	}

	if d.JSON {
		payload := map[string]any{
			"backend": backendName,
			"checks":  checks,
		}
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	_, err = fmt.Fprintf(ctx.Stdout, "doctor report (%s)\n", backendName)
	if err != nil {
		return err
	}
	for _, check := range checks {
		if _, err := fmt.Fprintf(ctx.Stdout, "- [%s] %s: %s\n", check.Status, check.Name, check.Message); err != nil {
			return err
		}
	}
	return nil
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

func mergeFirecrackerConfig(cwd string, e *ExecCommand, cfg runtimeconfig.Config) backend.FirecrackerConfig {
	out := backend.FirecrackerConfig{
		BinaryPath:      cfg.Backends.Firecracker.BinaryPath,
		KernelImagePath: cfg.Backends.Firecracker.KernelImage,
		RootFSPath:      cfg.Backends.Firecracker.RootFS,
		WorkspaceHost:   cwd,
		WorkspaceAccess: resolveWorkspaceAccess(e, cfg.Workspace.Access),
		VCPUs:           cfg.Backends.Firecracker.VCPUs,
		MemoryMiB:       cfg.Backends.Firecracker.MemoryMiB,
		GuestCID:        cfg.Backends.Firecracker.GuestCID,
		GuestPort:       cfg.Backends.Firecracker.GuestPort,
		LaunchSeconds:   cfg.Backends.Firecracker.LaunchSeconds,
	}

	if e.RunDir != "" {
		out.RunDir = e.RunDir
	}
	out.Launch = true
	if e.DryRun || e.HostPassthrough {
		out.Launch = false
	}
	out.HostPassthrough = e.HostPassthrough
	if e.LaunchSeconds != 0 {
		out.LaunchSeconds = e.LaunchSeconds
	}
	return out
}

func resolveWorkspaceAccess(execCfg *ExecCommand, configured string) string {
	access := configured
	if access == "" {
		access = "rw"
	}
	if execCfg != nil && execCfg.ReadOnlyWorkspace {
		access = "ro"
	}
	return access
}

func (s *StatusCommand) Run(ctx *runtimeContext) error {
	baseDir, err := paths.RunBaseDir()
	if err != nil {
		return fmt.Errorf("resolve run base directory: %w", err)
	}
	if s.RunID != "" && s.LastRun {
		return errors.New("choose either --run-id or --last-run")
	}
	if s.RunID != "" {
		return inspectRun(ctx.Stdout, baseDir, s.RunID)
	}
	if s.LastRun {
		entries, err := os.ReadDir(baseDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				_, werr := fmt.Fprintf(ctx.Stdout, "no runs found (%s does not exist)\n", baseDir)
				return werr
			}
			return err
		}
		var newest string
		var newestTime time.Time
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if newest == "" || info.ModTime().After(newestTime) {
				newest = entry.Name()
				newestTime = info.ModTime()
			}
		}
		if newest == "" {
			_, err := fmt.Fprintf(ctx.Stdout, "no runs found in %s\n", baseDir)
			return err
		}
		return inspectRun(ctx.Stdout, baseDir, newest)
	}

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, werr := fmt.Fprintf(ctx.Stdout, "no runs found (%s does not exist)\n", baseDir)
			return werr
		}
		return err
	}

	if len(entries) == 0 {
		_, err := fmt.Fprintf(ctx.Stdout, "no runs found in %s\n", baseDir)
		return err
	}

	_, err = fmt.Fprintf(ctx.Stdout, "runs in %s:\n", baseDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := fmt.Fprintf(ctx.Stdout, "- %s\n", entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func inspectRun(stdout *os.File, baseDir, runID string) error {
	runDir := filepath.Join(baseDir, runID)
	if _, err := os.Stat(runDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("run %q not found in %s", runID, baseDir)
		}
		return err
	}
	if _, err := fmt.Fprintf(stdout, "run: %s\n", runDir); err != nil {
		return err
	}
	obsPath := filepath.Join(runDir, "run-observability.json")
	b, err := os.ReadFile(obsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, werr := fmt.Fprintf(stdout, "observability: not found (%s)\n", obsPath)
			return werr
		}
		return err
	}
	var obs map[string]any
	if err := json.Unmarshal(b, &obs); err != nil {
		return fmt.Errorf("parse %s: %w", obsPath, err)
	}
	out, err := json.MarshalIndent(obs, "", "  ")
	if err != nil {
		return fmt.Errorf("format %s: %w", obsPath, err)
	}
	_, err = fmt.Fprintf(stdout, "observability (%s):\n%s\n", obsPath, out)
	return err
}

func resolveCWD(base, chdir string) (string, error) {
	if chdir == "" {
		return base, nil
	}
	if filepath.IsAbs(chdir) {
		return filepath.Clean(chdir), nil
	}
	return filepath.Join(base, chdir), nil
}

func newLogger(rawLevel, component string) (*log.Logger, error) {
	levelName := strings.TrimSpace(strings.ToLower(rawLevel))
	if levelName == "" {
		levelName = "info"
	}
	level, err := log.ParseLevel(levelName)
	if err != nil {
		return nil, fmt.Errorf("invalid --log-level %q: %w", rawLevel, err)
	}
	logger := log.NewWithOptions(os.Stderr, log.Options{
		Level:     level,
		Formatter: log.TextFormatter,
	})
	return logger.With("component", component), nil
}
