package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/alecthomas/kong"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/backend/firecracker"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
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
	Chdir string `short:"c" help:"Change to this directory before running commands"`

	Policy PolicyCommand `cmd:"" help:"Policy commands"`
	Exec   ExecCommand   `cmd:"" help:"Execute a command in a cleanroom backend"`
	Doctor DoctorCommand `cmd:"" help:"Run environment and backend diagnostics"`
	Status StatusCommand `cmd:"" help:"Inspect run artifacts"`
}

type PolicyCommand struct {
	Validate PolicyValidateCommand `cmd:"" help:"Validate policy configuration"`
}

type PolicyValidateCommand struct {
	JSON bool `help:"Print compiled policy as JSON"`
}

type ExecCommand struct {
	Backend string `help:"Execution backend (defaults to runtime config or firecracker)"`

	RunDir          string `help:"Run directory for generated artifacts (default: XDG runtime/state cleanroom path)"`
	DryRun          bool   `help:"Generate execution plan without running a backend command"`
	HostPassthrough bool   `help:"Run command directly on host instead of launching a backend (unsafe, not sandboxed)"`
	LaunchSeconds   int64  `help:"Launch/guest-exec timeout in seconds"`

	Command []string `arg:"" passthrough:"" required:"" help:"Command to execute"`
}

type StatusCommand struct {
	RunID string `help:"Run ID to inspect"`
}

type DoctorCommand struct {
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

	if cli.Chdir != "" {
		if err := os.Chdir(cli.Chdir); err != nil {
			return fmt.Errorf("change directory to %s: %w", cli.Chdir, err)
		}
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
	compiled, source, err := ctx.Loader.LoadAndCompile(ctx.CWD)
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
	backendName := resolveBackendName(e.Backend, ctx.Config.DefaultBackend)
	adapter, ok := ctx.Backends[backendName]
	if !ok {
		return fmt.Errorf("unknown backend %q", backendName)
	}

	command := normalizeCommand(e.Command)
	if len(command) == 0 {
		return errors.New("missing command")
	}

	compiled, source, err := ctx.Loader.LoadAndCompile(ctx.CWD)
	if err != nil {
		return err
	}

	runID := fmt.Sprintf("run-%d", time.Now().UTC().UnixNano())
	req := backend.RunRequest{
		RunID:             runID,
		CWD:               ctx.CWD,
		Command:           append([]string(nil), command...),
		Policy:            compiled,
		FirecrackerConfig: mergeFirecrackerConfig(e, ctx.Config.Backends.Firecracker),
	}

	result, err := adapter.Run(context.Background(), req)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(
		ctx.Stdout,
		"run id: %s\npolicy source: %s\npolicy hash: %s\nplan: %s\nrun dir: %s\nmessage: %s\n",
		result.RunID,
		source,
		compiled.Hash,
		result.PlanPath,
		result.RunDir,
		result.Message,
	)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return exitCodeError{code: result.ExitCode}
	}
	return nil
}

func (d *DoctorCommand) Run(ctx *runtimeContext) error {
	backendName := resolveBackendName(d.Backend, ctx.Config.DefaultBackend)
	adapter, ok := ctx.Backends[backendName]
	if !ok {
		return fmt.Errorf("unknown backend %q", backendName)
	}

	checks := []backend.DoctorCheck{
		{Name: "runtime_config", Status: "pass", Message: fmt.Sprintf("using runtime config path %s", ctx.ConfigPath)},
		{Name: "backend", Status: "pass", Message: fmt.Sprintf("selected backend %s", backendName)},
	}

	compiled, source, err := ctx.Loader.LoadAndCompile(ctx.CWD)
	if err != nil {
		checks = append(checks, backend.DoctorCheck{
			Name:    "repository_policy",
			Status:  "warn",
			Message: fmt.Sprintf("policy not loaded from %s: %v", ctx.CWD, err),
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
			FirecrackerConfig: mergeFirecrackerConfig(&ExecCommand{}, ctx.Config.Backends.Firecracker),
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

func mergeFirecrackerConfig(e *ExecCommand, cfg runtimeconfig.FirecrackerConfig) backend.FirecrackerConfig {
	out := backend.FirecrackerConfig{
		BinaryPath:      cfg.BinaryPath,
		KernelImagePath: cfg.KernelImage,
		RootFSPath:      cfg.RootFS,
		VCPUs:           cfg.VCPUs,
		MemoryMiB:       cfg.MemoryMiB,
		GuestCID:        cfg.GuestCID,
		GuestPort:       cfg.GuestPort,
		RetainWrites:    cfg.RetainWrites,
		LaunchSeconds:   cfg.LaunchSeconds,
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

func (s *StatusCommand) Run(ctx *runtimeContext) error {
	baseDir, err := paths.RunBaseDir()
	if err != nil {
		return fmt.Errorf("resolve run base directory: %w", err)
	}
	if s.RunID != "" {
		runDir := filepath.Join(baseDir, s.RunID)
		if _, err := os.Stat(runDir); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("run %q not found in %s", s.RunID, baseDir)
			}
			return err
		}
		_, err := fmt.Fprintf(ctx.Stdout, "run: %s\n", runDir)
		return err
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
