package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/alecthomas/kong"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/backend/firecracker"
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/charmbracelet/log"
	"golang.org/x/term"
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
	Policy  PolicyCommand  `cmd:"" help:"Policy commands"`
	Exec    ExecCommand    `cmd:"" help:"Execute a command in a cleanroom backend"`
	Console ConsoleCommand `cmd:"" help:"Attach an interactive console to a cleanroom execution"`
	Serve   ServeCommand   `cmd:"" help:"Run the cleanroom control-plane server"`
	Doctor  DoctorCommand  `cmd:"" help:"Run environment and backend diagnostics"`
	Status  StatusCommand  `cmd:"" help:"Inspect run artifacts"`
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

	ReadOnlyWorkspace bool  `help:"Mount workspace read-only for this run"`
	LaunchSeconds     int64 `help:"VM boot/guest-agent readiness timeout in seconds"`

	Command []string `arg:"" passthrough:"" required:"" help:"Command to execute"`
}

type ConsoleCommand struct {
	Chdir    string `short:"c" help:"Change to this directory before running commands"`
	Host     string `help:"Control-plane endpoint (unix://path, http://host:port, or https://host:port)"`
	LogLevel string `help:"Client log level (debug|info|warn|error)"`
	Backend  string `help:"Execution backend (defaults to runtime config or firecracker)"`

	ReadOnlyWorkspace bool  `help:"Mount workspace read-only for this run"`
	LaunchSeconds     int64 `help:"VM boot/guest-agent readiness timeout in seconds"`

	Command []string `arg:"" passthrough:"" optional:"" help:"Command to run in the console (default: sh)"`
}

type ServeCommand struct {
	Listen   string `help:"Listen endpoint for control API (defaults to runtime endpoint; supports tsnet://hostname[:port])"`
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

var (
	newSignalChannel = func() chan os.Signal {
		return make(chan os.Signal, 2)
	}
	notifySignals = func(ch chan os.Signal, sig ...os.Signal) {
		signal.Notify(ch, sig...)
	}
	stopSignals = func(ch chan os.Signal) {
		signal.Stop(ch)
	}
)

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

	ep, err := endpoint.Resolve(e.Host)
	if err != nil {
		return err
	}
	var cwd string
	if ep.Scheme != "unix" {
		if e.Chdir == "" {
			return fmt.Errorf("remote endpoint %q requires an explicit -c/--chdir with an absolute path", ep.Address)
		}
		if !filepath.IsAbs(e.Chdir) {
			return fmt.Errorf("remote endpoint %q requires an absolute -c/--chdir path, got %q", ep.Address, e.Chdir)
		}
		cwd = filepath.Clean(e.Chdir)
	} else {
		var err error
		cwd, err = resolveCWD(ctx.CWD, e.Chdir)
		if err != nil {
			return err
		}
	}
	logger.Debug("sending execution request",
		"endpoint", ep.Address,
		"cwd", cwd,
		"backend", e.Backend,
		"command_argc", len(e.Command),
	)
	client := controlclient.New(ep)
	createSandboxResp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Cwd:     cwd,
		Backend: e.Backend,
		Options: &cleanroomv1.SandboxOptions{
			ReadOnlyWorkspace: e.ReadOnlyWorkspace,
			LaunchSeconds:     e.LaunchSeconds,
		},
	})
	if err != nil {
		return fmt.Errorf("execute via control-plane endpoint %q: %w", ep.Address, err)
	}

	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()
	detached := false
	defer func() {
		if detached || sandboxID == "" {
			return
		}
		_, _ = client.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: sandboxID})
	}()

	createExecutionResp, err := client.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   append([]string(nil), e.Command...),
		Options: &cleanroomv1.ExecutionOptions{
			ReadOnlyWorkspace: e.ReadOnlyWorkspace,
			LaunchSeconds:     e.LaunchSeconds,
			Cwd:               cwd,
		},
	})
	if err != nil {
		return fmt.Errorf("create execution via control-plane endpoint %q: %w", ep.Address, err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	if _, err := fmt.Fprintf(os.Stderr, "sandbox_id=%s execution_id=%s\n", sandboxID, executionID); err != nil {
		return err
	}

	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	stream, err := client.StreamExecution(streamCtx, &cleanroomv1.StreamExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Follow:      true,
	})
	if err != nil {
		return fmt.Errorf("stream execution via control-plane endpoint %q: %w", ep.Address, err)
	}

	signalCh := newSignalChannel()
	notifySignals(signalCh, os.Interrupt, syscall.SIGTERM)
	defer stopSignals(signalCh)

	secondInterrupt := make(chan struct{}, 1)
	go func() {
		interrupts := 0
		for range signalCh {
			interrupts++
			if interrupts == 1 {
				cancelResp, cancelErr := client.CancelExecution(context.Background(), &cleanroomv1.CancelExecutionRequest{
					SandboxId:   sandboxID,
					ExecutionId: executionID,
					Signal:      2,
				})
				if cancelErr != nil && logger != nil {
					logger.Warn("cancel execution request failed", "sandbox_id", sandboxID, "execution_id", executionID, "error", cancelErr)
				} else if logger != nil && cancelResp != nil {
					logger.Debug("cancel execution requested",
						"sandbox_id", sandboxID,
						"execution_id", executionID,
						"accepted", cancelResp.GetAccepted(),
						"status", cancelResp.GetStatus().String(),
					)
				}
				continue
			}

			select {
			case secondInterrupt <- struct{}{}:
			default:
			}
			streamCancel()
			return
		}
	}()

	var exitCode int
	haveExitCode := false
	for stream.Receive() {
		event := stream.Msg()
		switch payload := event.Payload.(type) {
		case *cleanroomv1.ExecutionStreamEvent_Stdout:
			if _, err := fmt.Fprint(ctx.Stdout, string(payload.Stdout)); err != nil {
				return err
			}
		case *cleanroomv1.ExecutionStreamEvent_Stderr:
			if _, err := fmt.Fprint(os.Stderr, string(payload.Stderr)); err != nil {
				return err
			}
		case *cleanroomv1.ExecutionStreamEvent_Exit:
			exitCode = int(payload.Exit.GetExitCode())
			haveExitCode = true
		}
	}

	streamErr := stream.Err()
	select {
	case <-secondInterrupt:
		detached = true
		terminateCtx, terminateCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, terminateErr := client.TerminateSandbox(terminateCtx, &cleanroomv1.TerminateSandboxRequest{SandboxId: sandboxID})
		terminateCancel()
		if terminateErr != nil && logger != nil {
			logger.Warn("terminate sandbox after detach failed", "sandbox_id", sandboxID, "error", terminateErr)
		}
		return exitCodeError{code: 130}
	default:
	}

	if streamErr != nil && !isCanceledStreamErr(streamErr) {
		return fmt.Errorf("stream execution: %w", streamErr)
	}

	if !haveExitCode {
		getResp, getErr := client.GetExecution(context.Background(), &cleanroomv1.GetExecutionRequest{
			SandboxId:   sandboxID,
			ExecutionId: executionID,
		})
		if getErr == nil && getResp.GetExecution() != nil && isFinalExecutionStatus(getResp.GetExecution().GetStatus()) {
			exitCode = int(getResp.GetExecution().GetExitCode())
			haveExitCode = true
		}
	}

	logger.Debug("execution complete",
		"sandbox_id", sandboxID,
		"execution_id", executionID,
		"have_exit_code", haveExitCode,
		"exit_code", exitCode,
	)

	if !haveExitCode {
		return errors.New("execution stream ended without exit status")
	}
	if exitCode != 0 {
		return exitCodeError{code: exitCode}
	}
	return nil
}

type attachFrameSender struct {
	mu          sync.Mutex
	sandboxID   string
	executionID string
	stream      *connect.BidiStreamForClient[cleanroomv1.ExecutionAttachFrame, cleanroomv1.ExecutionAttachFrame]
}

func (s *attachFrameSender) Send(frame *cleanroomv1.ExecutionAttachFrame) error {
	if frame == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if frame.SandboxId == "" {
		frame.SandboxId = s.sandboxID
	}
	if frame.ExecutionId == "" {
		frame.ExecutionId = s.executionID
	}
	return s.stream.Send(frame)
}

func (c *ConsoleCommand) Run(ctx *runtimeContext) error {
	logger, err := newLogger(c.LogLevel, "client")
	if err != nil {
		return err
	}

	ep, err := endpoint.Resolve(c.Host)
	if err != nil {
		return err
	}
	var cwd string
	if ep.Scheme != "unix" {
		if c.Chdir == "" {
			return fmt.Errorf("remote endpoint %q requires an explicit -c/--chdir with an absolute path", ep.Address)
		}
		if !filepath.IsAbs(c.Chdir) {
			return fmt.Errorf("remote endpoint %q requires an absolute -c/--chdir path, got %q", ep.Address, c.Chdir)
		}
		cwd = filepath.Clean(c.Chdir)
	} else {
		var err error
		cwd, err = resolveCWD(ctx.CWD, c.Chdir)
		if err != nil {
			return err
		}
	}
	command := append([]string(nil), c.Command...)
	if len(command) == 0 {
		command = []string{"sh"}
	}
	logger.Debug("starting interactive console",
		"endpoint", ep.Address,
		"cwd", cwd,
		"backend", c.Backend,
		"command_argc", len(command),
	)

	client := controlclient.New(ep)
	createSandboxResp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Cwd:     cwd,
		Backend: c.Backend,
		Options: &cleanroomv1.SandboxOptions{
			ReadOnlyWorkspace: c.ReadOnlyWorkspace,
			LaunchSeconds:     c.LaunchSeconds,
		},
	})
	if err != nil {
		return fmt.Errorf("console via control-plane endpoint %q: %w", ep.Address, err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()
	defer func() {
		if sandboxID == "" {
			return
		}
		_, _ = client.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: sandboxID})
	}()

	createExecutionResp, err := client.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   command,
		Options: &cleanroomv1.ExecutionOptions{
			ReadOnlyWorkspace: c.ReadOnlyWorkspace,
			LaunchSeconds:     c.LaunchSeconds,
			Tty:               true,
			Cwd:               cwd,
		},
	})
	if err != nil {
		return fmt.Errorf("create console execution via control-plane endpoint %q: %w", ep.Address, err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()
	if _, err := fmt.Fprintf(os.Stderr, "sandbox_id=%s execution_id=%s\n", sandboxID, executionID); err != nil {
		return err
	}

	attachCtx, attachCancel := context.WithCancel(context.Background())
	defer attachCancel()
	attach := client.AttachExecution(attachCtx)
	sender := &attachFrameSender{
		sandboxID:   sandboxID,
		executionID: executionID,
		stream:      attach,
	}
	if err := sender.Send(&cleanroomv1.ExecutionAttachFrame{
		Payload: &cleanroomv1.ExecutionAttachFrame_Open{
			Open: &cleanroomv1.ExecutionAttachOpen{
				SandboxId:   sandboxID,
				ExecutionId: executionID,
			},
		},
	}); err != nil {
		return fmt.Errorf("open attach stream: %w", err)
	}

	stdinFD := int(os.Stdin.Fd())
	rawMode := false
	if term.IsTerminal(stdinFD) {
		oldState, rawErr := term.MakeRaw(stdinFD)
		if rawErr != nil {
			logger.Warn("failed to enter raw mode", "error", rawErr)
		} else {
			rawMode = true
			defer func() {
				_ = term.Restore(stdinFD, oldState)
			}()
			if cols, rows, sizeErr := term.GetSize(stdinFD); sizeErr == nil {
				_ = sender.Send(&cleanroomv1.ExecutionAttachFrame{
					Payload: &cleanroomv1.ExecutionAttachFrame_Resize{
						Resize: &cleanroomv1.ExecutionResize{
							Cols: uint32(cols),
							Rows: uint32(rows),
						},
					},
				})
			}
		}
	}

	signalCh := newSignalChannel()
	notifySignals(signalCh, os.Interrupt, syscall.SIGTERM)
	defer stopSignals(signalCh)

	if rawMode {
		resizeSignalCh := make(chan os.Signal, 4)
		signal.Notify(resizeSignalCh, syscall.SIGWINCH)
		defer signal.Stop(resizeSignalCh)
		go func() {
			for range resizeSignalCh {
				cols, rows, sizeErr := term.GetSize(stdinFD)
				if sizeErr != nil {
					continue
				}
				_ = sender.Send(&cleanroomv1.ExecutionAttachFrame{
					Payload: &cleanroomv1.ExecutionAttachFrame_Resize{
						Resize: &cleanroomv1.ExecutionResize{
							Cols: uint32(cols),
							Rows: uint32(rows),
						},
					},
				})
			}
		}()
	}

	go func() {
		for sig := range signalCh {
			num := int32(2)
			if sig == syscall.SIGTERM {
				num = 15
			}
			_ = sender.Send(&cleanroomv1.ExecutionAttachFrame{
				Payload: &cleanroomv1.ExecutionAttachFrame_Signal{
					Signal: &cleanroomv1.ExecutionSignal{Signal: num},
				},
			})
		}
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := os.Stdin.Read(buf)
			if n > 0 {
				payload := append([]byte(nil), buf[:n]...)
				if sendErr := sender.Send(&cleanroomv1.ExecutionAttachFrame{
					Payload: &cleanroomv1.ExecutionAttachFrame_Stdin{Stdin: payload},
				}); sendErr != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	var exitCode int
	haveExitCode := false
	for {
		frame, recvErr := attach.Receive()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) || isCanceledStreamErr(recvErr) {
				break
			}
			return fmt.Errorf("attach execution: %w", recvErr)
		}
		switch payload := frame.Payload.(type) {
		case *cleanroomv1.ExecutionAttachFrame_Stdout:
			if _, err := fmt.Fprint(ctx.Stdout, string(payload.Stdout)); err != nil {
				return err
			}
		case *cleanroomv1.ExecutionAttachFrame_Stderr:
			if _, err := fmt.Fprint(os.Stderr, string(payload.Stderr)); err != nil {
				return err
			}
		case *cleanroomv1.ExecutionAttachFrame_Exit:
			exitCode = int(payload.Exit.GetExitCode())
			haveExitCode = true
		case *cleanroomv1.ExecutionAttachFrame_Error:
			_ = payload
		}
	}

	if !haveExitCode {
		getResp, getErr := client.GetExecution(context.Background(), &cleanroomv1.GetExecutionRequest{
			SandboxId:   sandboxID,
			ExecutionId: executionID,
		})
		if getErr == nil && getResp.GetExecution() != nil && isFinalExecutionStatus(getResp.GetExecution().GetStatus()) {
			exitCode = int(getResp.GetExecution().GetExitCode())
			haveExitCode = true
		}
	}

	if !haveExitCode {
		return errors.New("console stream ended without exit status")
	}
	if exitCode != 0 {
		return exitCodeError{code: exitCode}
	}
	return nil
}

func (s *ServeCommand) Run(ctx *runtimeContext) error {
	logger, err := newLogger(s.LogLevel, "server")
	if err != nil {
		return err
	}

	ep, err := endpoint.ResolveListen(s.Listen)
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

	out.Launch = true
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

func isCanceledStreamErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	var connectErr *connect.Error
	if errors.As(err, &connectErr) && connectErr.Code() == connect.CodeCanceled {
		return true
	}
	return false
}

func isFinalExecutionStatus(status cleanroomv1.ExecutionStatus) bool {
	switch status {
	case cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED,
		cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED,
		cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED,
		cleanroomv1.ExecutionStatus_EXECUTION_STATUS_TIMED_OUT:
		return true
	default:
		return false
	}
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
