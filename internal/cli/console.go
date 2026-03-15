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
	"golang.org/x/term"
)

type ConsoleCommand struct {
	clientFlags
	Chdir          string `short:"c" help:"Change to this directory before running commands"`
	Backend        string `help:"Execution backend (defaults to runtime config or host default)"`
	In             string `name:"in" aliases:"sandbox-id" help:"Run in an existing sandbox ID instead of creating a new one"`
	From           string `name:"from" help:"Create the sandbox from an existing snapshot ID"`
	Image          string `help:"Override sandbox image ref for newly created sandboxes (tag, digest, or local Docker image)"`
	Keep           bool   `help:"Keep a newly created sandbox after the console exits"`
	PrintSandboxID bool   `name:"print-sandbox-id" help:"Print resolved sandbox_id=<id> to stderr before attaching"`

	LaunchSeconds int64 `help:"VM boot/guest-agent readiness timeout in seconds"`

	Command []string `arg:"" passthrough:"" optional:"" help:"Command to run in the console (default: sh)"`
}

func (c *ConsoleCommand) Run(ctx *runtimeContext) error {
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

	command := append([]string(nil), c.Command...)
	if len(command) == 0 {
		command = []string{"sh"}
	}
	logger.Debug("starting interactive console",
		"host", host,
		"backend", c.Backend,
		"sandbox_id", strings.TrimSpace(c.In),
		"command_argc", len(command),
	)
	persistentRepositoryBackend := backendSupportsRepositoryPersistence(ctx, host, c.Backend)
	repository, err := maybeResolveRepositoryCheckout(cwd, ctx.Loader, strings.TrimSpace(c.In), strings.TrimSpace(c.From), !persistentRepositoryBackend)
	if err != nil {
		return err
	}
	warnDirtyRepositoryCheckout(repository)
	inlineRepositoryBootstrap := shouldInlineRepositoryBootstrap(ctx, host, c.Backend, repository)
	repositoryForCreate := repository
	if inlineRepositoryBootstrap {
		repositoryForCreate = nil
	}
	if strings.TrimSpace(c.In) != "" && c.Keep {
		return errors.New("--keep cannot be used with --in")
	}
	sandboxID, createdSandbox, err := ensureSandboxID(client, ctx.Loader, cwd, host, c.Backend, strings.TrimSpace(c.In), strings.TrimSpace(c.From), c.Image, c.LaunchSeconds, repositoryForCreate)
	if err != nil {
		if strings.TrimSpace(c.From) != "" {
			err = explainSnapshotRuntimeDisabledError(err, ctx)
		}
		return err
	}
	if c.PrintSandboxID {
		if _, err := fmt.Fprintf(os.Stderr, "sandbox_id=%s\n", sandboxID); err != nil {
			return err
		}
	}
	autoTerminateSandbox := createdSandbox && !c.Keep
	defer func() {
		if sandboxID == "" || !autoTerminateSandbox {
			return
		}
		terminateSandboxBestEffort(client, sandboxID, 0, logger, "terminate sandbox after console failed")
	}()

	createExecutionResp, err := client.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId:          sandboxID,
		Command:            repositoryExecutionCommand(command, repository, inlineRepositoryBootstrap),
		Kind:               cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE,
		RepositoryCheckout: repositoryExecutionCheckout(repository, inlineRepositoryBootstrap),
		Options: &cleanroomv1.ExecutionOptions{
			LaunchSeconds: c.LaunchSeconds,
			Tty:           true,
		},
	})
	if err != nil {
		return fmt.Errorf("create execution: %w", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()
	logger.Debug("console execution started", "sandbox_id", sandboxID, "execution_id", executionID)

	stdinFD := int(os.Stdin.Fd())
	initialCols, initialRows := attachTTYSize(stdinFD)
	openResp, err := client.OpenInteractiveExecution(context.Background(), &cleanroomv1.OpenInteractiveExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		InitialCols: initialCols,
		InitialRows: initialRows,
	})
	if err != nil {
		if isExecutionNoLongerActiveErr(err) {
			exitCode, haveExitCode, replayErr := replayExecutionHistory(client, sandboxID, executionID, ctx.Stdout, os.Stderr)
			if replayErr != nil {
				return fmt.Errorf("open interactive execution: %w", err)
			}
			if !haveExitCode {
				if fetchedExitCode, ok := getFinalExecutionExitCode(client, sandboxID, executionID); ok {
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
		return fmt.Errorf("open interactive execution: %w", err)
	}
	controlEndpoint, err := endpoint.Resolve(host)
	if err != nil {
		return err
	}
	quicEndpoint := resolveInteractiveDialEndpoint(controlEndpoint, openResp.GetQuicEndpoint())
	interactiveSession, err := interactivequic.Dial(
		context.Background(),
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
				return replayExecutionHistory(client, sandboxID, executionID, ctx.Stdout, os.Stderr)
			},
			func() (int, bool) {
				return getFinalExecutionExitCode(client, sandboxID, executionID)
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
		if fetchedExitCode, ok := getFinalExecutionExitCode(client, sandboxID, executionID); ok {
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
