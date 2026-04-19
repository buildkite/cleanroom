package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/interactivequic"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/term"
)

type ConsoleCommand struct {
	clientFlags
	Chdir   string `short:"c" help:"Change to this directory before running commands"`
	Backend string `help:"Execution backend (defaults to runtime config or host default)"`
	In      string `name:"in" aliases:"sandbox-id" help:"Run in an existing sandbox ID instead of creating a new one"`
	From    string `name:"from" help:"Create the sandbox from an existing snapshot ID"`
	Image   string `help:"Override sandbox image ref for newly created sandboxes (tag, digest, or local Docker image)"`
	repositoryOverrideFlags
	repositoryChangesetFlags
	Keep           bool     `help:"Keep a newly created sandbox after the console exits"`
	Env            []string `short:"e" name:"env" help:"Set guest environment variables; use KEY to inherit from the local environment or KEY=VALUE to set an explicit value"`
	PrintSandboxID bool     `name:"print-sandbox-id" help:"Print resolved sandbox_id=<id> to stderr before attaching"`

	LaunchSeconds int64 `help:"VM boot/guest-agent readiness timeout in seconds"`

	Command []string `arg:"" passthrough:"partial" optional:"" help:"Command to run in the console (default: sh)"`
}

func (c *ConsoleCommand) Run(ctx *runtimeContext) (runErr error) {
	if err := validateExecutionSandboxArgs(c.Chdir, c.In, c.From, c.Keep, c.repositoryOverrideFlags, c.repositoryChangesetFlags); err != nil {
		return err
	}

	sandboxID := ""
	executionID := ""
	command := append([]string(nil), c.Command...)
	if len(command) == 0 {
		command = []string{"sh"}
	}
	commandArgs := executionCommandArgs(command)
	rootAttrs := []attribute.KeyValue{
		attribute.String("cleanroom.backend.requested", strings.TrimSpace(c.Backend)),
		attribute.Bool("cleanroom.keep_sandbox", c.Keep),
		attribute.Int("cleanroom.command.argc", len(commandArgs)),
	}
	if commandName := executionCommandName(commandArgs); commandName != "" {
		rootAttrs = append(rootAttrs,
			attribute.String("cleanroom.command.name", commandName),
			attribute.String("cleanroom.command.summary", executionCommandSummary(commandArgs)),
		)
	}
	rootCtx, rootSpan := ctx.Observability.Tracer("github.com/buildkite/cleanroom/internal/cli").Start(
		context.Background(),
		"cleanroom.console",
		trace.WithAttributes(rootAttrs...),
	)
	traceID := traceIDFromContext(rootCtx)
	defer func() {
		if sandboxID != "" {
			rootSpan.SetAttributes(attribute.String("cleanroom.sandbox.id", sandboxID))
		}
		if executionID != "" {
			rootSpan.SetAttributes(attribute.String("cleanroom.execution.id", executionID))
		}
		if runErr != nil {
			rootSpan.RecordError(runErr)
			rootSpan.SetStatus(codes.Error, runErr.Error())
		} else {
			rootSpan.SetStatus(codes.Ok, "")
		}
		rootSpan.End()
	}()

	logger, err := newLogger(c.LogLevel, "client")
	if err != nil {
		return err
	}

	host := c.resolvedHost(ctx.Config)
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}
	cwd, err := resolveCWD(ctx.CWD, c.Chdir)
	if err != nil {
		return err
	}
	executionEnv, err := resolveExecutionEnv(c.Env)
	if err != nil {
		return err
	}

	logger.Debug("starting interactive console",
		"host", host,
		"backend", c.Backend,
		"sandbox_id", strings.TrimSpace(c.In),
		"command_argc", len(command),
		"env_count", len(executionEnv),
	)
	target, err := resolveExecutionSandbox(rootCtx, logger, client, ctx, cwd, host, c.Backend, c.In, c.From, c.Image, c.LaunchSeconds, c.repositoryOverrideFlags, c.repositoryChangesetFlags)
	if err != nil {
		if strings.TrimSpace(c.From) != "" {
			err = explainSnapshotRuntimeDisabledError(err, ctx)
		}
		return err
	}
	sandboxID = target.SandboxID
	createdSandbox := target.CreatedSandbox
	repository := target.Repository
	printedSandboxID := false
	printSandboxID := func() error {
		if printedSandboxID {
			return nil
		}
		if err := writeSandboxID(os.Stderr, sandboxID); err != nil {
			return err
		}
		printedSandboxID = true
		return nil
	}
	printedExecutionID := false
	printExecutionID := func() error {
		if printedExecutionID {
			return nil
		}
		if err := writeExecutionID(os.Stderr, executionID); err != nil {
			return err
		}
		printedExecutionID = true
		return nil
	}
	printedTraceID := false
	printTraceID := func() error {
		if printedTraceID {
			return nil
		}
		if err := writeTraceID(os.Stderr, traceID); err != nil {
			return err
		}
		printedTraceID = true
		return nil
	}
	printedTraceURL := false
	printTraceURL := func() error {
		if printedTraceURL {
			return nil
		}
		traceURL, err := runtimeconfig.RenderTraceURL(ctx.Config.Observability, traceID, executionID, sandboxID)
		if err != nil {
			return err
		}
		if err := writeTraceURL(os.Stderr, traceURL); err != nil {
			return err
		}
		if strings.TrimSpace(traceURL) != "" {
			printedTraceURL = true
		}
		return nil
	}
	defer func() {
		if !createdSandbox || !c.Keep {
			return
		}
		if err := printSandboxID(); err != nil {
			if runErr == nil {
				runErr = err
				return
			}
			runErr = errors.Join(runErr, err)
		}
	}()
	defer func() {
		if runErr == nil {
			return
		}
		var extraErr error
		if err := printSandboxID(); err != nil {
			extraErr = errors.Join(extraErr, err)
		}
		if err := printExecutionID(); err != nil {
			extraErr = errors.Join(extraErr, err)
		}
		if err := printTraceID(); err != nil {
			extraErr = errors.Join(extraErr, err)
		}
		if err := printTraceURL(); err != nil {
			extraErr = errors.Join(extraErr, err)
		}
		if sandboxID != "" && executionID != "" {
			if err := writeExecutionInspectCommand(os.Stderr, sandboxID, executionID); err != nil {
				extraErr = errors.Join(extraErr, err)
			}
			resp, err := client.InspectExecution(rootCtx, &cleanroomv1.InspectExecutionRequest{
				SandboxId:   sandboxID,
				ExecutionId: executionID,
			})
			if err == nil {
				if err := writeArtifactsDir(os.Stderr, resp.GetArtifactsDir()); err != nil {
					extraErr = errors.Join(extraErr, err)
				}
			}
		}
		if extraErr != nil {
			runErr = errors.Join(runErr, extraErr)
		}
	}()
	if c.PrintSandboxID {
		if err := printSandboxID(); err != nil {
			return err
		}
	}
	autoTerminateSandbox := createdSandbox && !c.Keep
	defer func() {
		if sandboxID == "" || !autoTerminateSandbox {
			return
		}
		terminateSandboxBestEffort(rootCtx, client, sandboxID, 0, logger, "terminate sandbox after console failed")
	}()

	createExecutionResp, err := client.CreateExecution(rootCtx, &cleanroomv1.CreateExecutionRequest{
		SandboxId:          sandboxID,
		Command:            repositorycheckout.NormalizeCommand(command),
		Env:                executionEnv,
		Kind:               cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE,
		RepositoryCheckout: repositoryCheckoutProto(repository),
		Options: &cleanroomv1.ExecutionOptions{
			LaunchSeconds: c.LaunchSeconds,
			Tty:           true,
		},
	})
	if err != nil {
		return fmt.Errorf("create execution: %w", err)
	}
	executionID = createExecutionResp.GetExecution().GetExecutionId()
	logger.Debug("console execution started", "sandbox_id", sandboxID, "execution_id", executionID)

	stdinFD := int(os.Stdin.Fd())
	initialCols, initialRows := attachTTYSize(stdinFD)
	openResp, err := client.AttachExecution(rootCtx, &cleanroomv1.AttachExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		InitialCols: initialCols,
		InitialRows: initialRows,
	})
	if err != nil {
		if isExecutionNoLongerActiveErr(err) {
			exitCode, haveExitCode, replayErr := replayExecutionHistory(rootCtx, client, sandboxID, executionID, ctx.Stdout, os.Stderr)
			if replayErr != nil {
				return fmt.Errorf("attach interactive console: %w", err)
			}
			if !haveExitCode {
				if fetchedExitCode, ok := getFinalExecutionExitCode(rootCtx, client, sandboxID, executionID); ok {
					exitCode = fetchedExitCode
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
		return fmt.Errorf("attach interactive console: %w", err)
	}
	controlEndpoint, err := endpoint.Resolve(host)
	if err != nil {
		return err
	}
	quicEndpoint := resolveInteractiveDialEndpoint(controlEndpoint, openResp.GetQuicEndpoint())
	interactiveSession, err := interactivequic.Dial(
		rootCtx,
		quicEndpoint,
		openResp.GetAlpn(),
		openResp.GetServerCertPinSha256(),
		openResp.GetSessionId(),
		openResp.GetSessionToken(),
	)
	if err != nil {
		return resolveConsoleDialFailure(
			err,
			func() (int, bool, error) {
				return replayExecutionHistory(rootCtx, client, sandboxID, executionID, ctx.Stdout, os.Stderr)
			},
			func() (int, bool) {
				return getFinalExecutionExitCode(rootCtx, client, sandboxID, executionID)
			},
		)
	}
	defer interactiveSession.Close()

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
				_ = interactiveSession.SendResize(uint32(cols), uint32(rows))
			}
		}
	}

	signalCh := newSignalChannel()
	notifySignals(signalCh, os.Interrupt, syscall.SIGTERM)
	defer stopSignals(signalCh)

	var interruptCount atomic.Int32
	var lastInterruptAt atomic.Int64
	forceLocalExit := make(chan struct{})
	var forceLocalExitOnce sync.Once
	requestInterrupt := func(signal int32) {
		now := time.Now()
		last := time.Unix(0, lastInterruptAt.Load())
		if !last.IsZero() && now.Sub(last) > interruptForceExitWindow {
			interruptCount.Store(0)
		}
		count := interruptCount.Add(1)
		lastInterruptAt.Store(now.UnixNano())
		if count == 1 {
			_ = interactiveSession.SendSignal(signal)
			return
		}
		forceLocalExitOnce.Do(func() {
			close(forceLocalExit)
			go func() {
				_ = interactiveSession.Close()
			}()
		})
	}
	isForceLocalExit := func() bool {
		select {
		case <-forceLocalExit:
			return true
		default:
			return false
		}
	}

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
				_ = interactiveSession.SendResize(uint32(cols), uint32(rows))
			}
		}()
	}

	go func() {
		for sig := range signalCh {
			num := int32(2)
			if sig == syscall.SIGTERM {
				num = 15
			}
			requestInterrupt(num)
		}
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := os.Stdin.Read(buf)
			if n > 0 {
				payload := append([]byte(nil), buf[:n]...)
				if rawMode {
					filtered := payload[:0]
					for _, b := range payload {
						if b == 0x03 {
							requestInterrupt(2)
							continue
						}
						filtered = append(filtered, b)
					}
					payload = filtered
					if isForceLocalExit() {
						return
					}
				}
				if len(payload) > 0 {
					if sendErr := interactiveSession.WriteStdin(payload); sendErr != nil {
						return
					}
				}
			}
			if readErr != nil {
				_ = interactiveSession.CloseStdin()
				return
			}
		}
	}()

	exitCodeCh := make(chan int, 1)
	controlErrCh := make(chan error, 1)
	go func() {
		defer close(controlErrCh)
		for msg := range interactiveSession.Events() {
			switch msg.Type {
			case "exit":
				select {
				case exitCodeCh <- int(msg.ExitCode):
				default:
				}
				return
			case "error":
				if strings.TrimSpace(msg.Error) == "" {
					continue
				}
				controlErrCh <- errors.New(msg.Error)
				return
			}
		}
		if err, ok := <-interactiveSession.EventErr(); ok && err != nil {
			controlErrCh <- err
		}
	}()

	var exitCode int
	haveExitCode := false
	endedCR := false
	buf := make([]byte, 4096)
	for {
		n, readErr := interactiveSession.ReadPTY(buf)
		if isForceLocalExit() {
			return exitCodeError{code: 130}
		}
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if rawMode {
				chunk, endedCR = normalizeLineEndingsForRawTTY(chunk, endedCR)
			}
			if _, err := ctx.Stdout.Write(chunk); err != nil {
				return err
			}
		}
		if polledExitCode, gotExitCode, pollErr := pollInteractiveExitOrControlErr(exitCodeCh, &controlErrCh); pollErr != nil {
			if isForceLocalExit() {
				return exitCodeError{code: 130}
			}
			return pollErr
		} else if gotExitCode {
			exitCode = polledExitCode
			haveExitCode = true
		}
		if readErr != nil {
			if !isInteractiveStreamClosedErr(readErr) {
				return fmt.Errorf("interactive pty stream: %w", readErr)
			}
			break
		}
	}
	if isForceLocalExit() {
		return exitCodeError{code: 130}
	}

	if !haveExitCode {
		waitedExitCode, gotExitCode, waitErr := waitForInteractiveExitOrControlErr(exitCodeCh, &controlErrCh, 2*time.Second)
		if waitErr != nil {
			if isForceLocalExit() {
				return exitCodeError{code: 130}
			}
			return waitErr
		}
		if gotExitCode {
			exitCode = waitedExitCode
			haveExitCode = true
		}
	}
	if isForceLocalExit() {
		return exitCodeError{code: 130}
	}

	if !haveExitCode {
		if fetchedExitCode, ok := getFinalExecutionExitCode(rootCtx, client, sandboxID, executionID); ok {
			exitCode = fetchedExitCode
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
