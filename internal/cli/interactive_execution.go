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

	"charm.land/log/v2"
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/interactivequic"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"golang.org/x/term"
)

type interactiveExecutionOptions struct {
	NoStdin bool
	Label   string
}

type interactiveExecutionResult struct {
	ExecutionID     string
	ForcedLocalExit bool
}

func requestInteractiveExecutionCancelAsync(
	rootCtx context.Context,
	logger *log.Logger,
	sandboxID, executionID string,
	signal int32,
	cancelExecution func(context.Context, *cleanroomv1.CancelExecutionRequest) (*cleanroomv1.CancelExecutionResponse, error),
) {
	if cancelExecution == nil {
		return
	}
	go func() {
		cancelResp, cancelErr := cancelExecution(tracePreservingContext(rootCtx), &cleanroomv1.CancelExecutionRequest{
			SandboxId:   sandboxID,
			ExecutionId: executionID,
			Signal:      signal,
		})
		if cancelErr != nil && logger != nil {
			logger.Warn("cancel interactive execution request failed", "sandbox_id", sandboxID, "execution_id", executionID, "error", cancelErr)
		} else if cancelResp != nil && !cancelResp.GetAccepted() && logger != nil {
			logger.Warn("cancel interactive execution request was not accepted", "sandbox_id", sandboxID, "execution_id", executionID, "status", cancelResp.GetStatus().String())
		}
	}()
}

func runInteractiveExecution(
	rootCtx context.Context,
	ctx *runtimeContext,
	client *controlclient.Client,
	logger *log.Logger,
	host, sandboxID string,
	repository *resolvedRepositoryCheckout,
	command, executionEnv []string,
	launchSeconds int64,
	opts interactiveExecutionOptions,
) (interactiveExecutionResult, error) {
	label := strings.TrimSpace(opts.Label)
	if label == "" {
		label = "execution"
	}

	createExecutionResp, err := client.CreateExecution(rootCtx, &cleanroomv1.CreateExecutionRequest{
		SandboxId:          sandboxID,
		Command:            repositorycheckout.NormalizeCommand(command),
		Env:                executionEnv,
		Kind:               cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE,
		RepositoryCheckout: repositoryCheckoutProto(repository),
		Options: &cleanroomv1.ExecutionOptions{
			LaunchSeconds: launchSeconds,
			Tty:           true,
		},
	})
	if err != nil {
		return interactiveExecutionResult{}, fmt.Errorf("create execution: %w", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()
	result := interactiveExecutionResult{ExecutionID: executionID}
	forcedLocalExitResult := func() (interactiveExecutionResult, error) {
		return interactiveExecutionResult{
			ExecutionID:     executionID,
			ForcedLocalExit: true,
		}, exitCodeError{code: 130}
	}

	interactiveCtx, interactiveCancel := context.WithCancel(rootCtx)
	defer interactiveCancel()

	signalCh := newSignalChannel()
	notifySignals(signalCh, os.Interrupt, syscall.SIGTERM)
	defer stopSignals(signalCh)

	var interruptCount atomic.Int32
	var lastInterruptAt atomic.Int64
	forceLocalExit := make(chan struct{})
	var forceLocalExitOnce sync.Once
	var sessionMu sync.RWMutex
	var interactiveSession *interactivequic.Session
	requestInterrupt := func(signal int32) {
		now := time.Now()
		last := time.Unix(0, lastInterruptAt.Load())
		if !last.IsZero() && now.Sub(last) > interruptForceExitWindow {
			interruptCount.Store(0)
		}
		count := interruptCount.Add(1)
		lastInterruptAt.Store(now.UnixNano())
		if count == 1 {
			sessionMu.RLock()
			session := interactiveSession
			sessionMu.RUnlock()
			if session != nil {
				_ = session.SendSignal(signal)
				return
			}
			requestInteractiveExecutionCancelAsync(rootCtx, logger, sandboxID, executionID, signal, client.CancelExecution)
			return
		}
		forceLocalExitOnce.Do(func() {
			close(forceLocalExit)
			interactiveCancel()
			sessionMu.RLock()
			session := interactiveSession
			sessionMu.RUnlock()
			if session != nil {
				go func() {
					_ = session.Close()
				}()
			}
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

	go func() {
		for sig := range signalCh {
			num := int32(2)
			if sig == syscall.SIGTERM {
				num = 15
			}
			requestInterrupt(num)
		}
	}()

	stdinFD := int(os.Stdin.Fd())
	initialCols, initialRows := attachTTYSize(stdinFD)
	openResp, err := client.AttachExecution(interactiveCtx, &cleanroomv1.AttachExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		InitialCols: initialCols,
		InitialRows: initialRows,
	})
	if err != nil {
		if isForceLocalExit() {
			return forcedLocalExitResult()
		}
		if isExecutionNoLongerActiveErr(err) {
			exitCode, haveExitCode, replayErr := replayExecutionHistory(rootCtx, client, sandboxID, executionID, ctx.Stdout, os.Stderr)
			if replayErr != nil {
				return result, fmt.Errorf("attach interactive %s: %w", label, err)
			}
			if !haveExitCode {
				if fetchedExitCode, ok := getFinalExecutionExitCode(rootCtx, client, sandboxID, executionID); ok {
					exitCode = fetchedExitCode
					haveExitCode = true
				}
			}
			if !haveExitCode {
				return result, fmt.Errorf("%s stream ended without exit status", label)
			}
			if exitCode != 0 {
				return result, exitCodeError{code: exitCode}
			}
			return result, nil
		}
		return result, fmt.Errorf("attach interactive %s: %w", label, err)
	}
	controlEndpoint, err := endpoint.Resolve(host)
	if err != nil {
		if isForceLocalExit() {
			return forcedLocalExitResult()
		}
		return result, err
	}
	quicEndpoint := resolveInteractiveDialEndpoint(controlEndpoint, openResp.GetQuicEndpoint())
	session, err := interactivequic.Dial(
		interactiveCtx,
		quicEndpoint,
		openResp.GetAlpn(),
		openResp.GetServerCertPinSha256(),
		openResp.GetSessionId(),
		openResp.GetSessionToken(),
	)
	if err != nil {
		if isForceLocalExit() {
			return forcedLocalExitResult()
		}
		return result, resolveConsoleDialFailure(
			err,
			func() (int, bool, error) {
				return replayExecutionHistory(rootCtx, client, sandboxID, executionID, ctx.Stdout, os.Stderr)
			},
			func() (int, bool) {
				return getFinalExecutionExitCode(rootCtx, client, sandboxID, executionID)
			},
		)
	}
	sessionMu.Lock()
	interactiveSession = session
	sessionMu.Unlock()
	defer func() {
		sessionMu.Lock()
		interactiveSession = nil
		sessionMu.Unlock()
	}()
	defer interactiveSession.Close()

	rawMode := false
	if !opts.NoStdin && term.IsTerminal(stdinFD) {
		oldState, rawErr := term.MakeRaw(stdinFD)
		if rawErr != nil {
			if logger != nil {
				logger.Warn("failed to enter raw mode", "error", rawErr)
			}
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

	if opts.NoStdin {
		if err := interactiveSession.CloseStdin(); err != nil && !isClosedNetworkConnectionErr(err) {
			if isForceLocalExit() {
				return forcedLocalExitResult()
			}
			return result, fmt.Errorf("close interactive stdin: %w", err)
		}
	}

	var stdinErrCh <-chan error
	var stdinSentInput atomic.Bool
	if !opts.NoStdin {
		errCh := make(chan error, 1)
		stdinErrCh = errCh
		go func() {
			defer close(errCh)
			reportErr := func(err error) {
				if err == nil || isForceLocalExit() {
					return
				}
				select {
				case errCh <- err:
				default:
				}
				_ = interactiveSession.Close()
			}

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
						stdinSentInput.Store(true)
						if sendErr := interactiveSession.WriteStdin(payload); sendErr != nil {
							reportErr(fmt.Errorf("write interactive stdin: %w", sendErr))
							return
						}
					}
				}
				if readErr != nil {
					if closeErr := interactiveSession.CloseStdin(); closeErr != nil && !isClosedNetworkConnectionErr(closeErr) {
						reportErr(fmt.Errorf("close interactive stdin: %w", closeErr))
					}
					return
				}
			}
		}()
	}

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
			return forcedLocalExitResult()
		}
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if rawMode {
				chunk, endedCR = normalizeLineEndingsForRawTTY(chunk, endedCR)
			}
			if _, err := ctx.Stdout.Write(chunk); err != nil {
				return result, err
			}
		}
		if stdinErr := pollExecutionStdinErr(stdinErrCh); stdinErr != nil {
			if isForceLocalExit() {
				return forcedLocalExitResult()
			}
			return result, stdinErr
		}
		if polledExitCode, gotExitCode, pollErr := pollInteractiveExitOrControlErr(exitCodeCh, &controlErrCh); pollErr != nil {
			if isForceLocalExit() {
				return forcedLocalExitResult()
			}
			return result, pollErr
		} else if gotExitCode {
			exitCode = polledExitCode
			haveExitCode = true
		}
		if stdinErr := pollExecutionStdinErr(stdinErrCh); stdinErr != nil {
			if isForceLocalExit() {
				return forcedLocalExitResult()
			}
			return result, stdinErr
		}
		if readErr != nil {
			if !isInteractiveStreamClosedErr(readErr) {
				return result, fmt.Errorf("interactive pty stream: %w", readErr)
			}
			break
		}
	}
	if isForceLocalExit() {
		return forcedLocalExitResult()
	}

	if !haveExitCode {
		waitedExitCode, gotExitCode, waitErr := waitForInteractiveExitOrControlErr(exitCodeCh, &controlErrCh, 2*time.Second)
		if waitErr != nil {
			if isForceLocalExit() {
				return forcedLocalExitResult()
			}
			return result, waitErr
		}
		if gotExitCode {
			exitCode = waitedExitCode
			haveExitCode = true
		}
	}
	if isForceLocalExit() {
		return forcedLocalExitResult()
	}
	if stdinErr := waitExecutionStdinErr(stdinErrCh, executionStdinErrDrainTimeout); stdinErr != nil {
		if isForceLocalExit() {
			return forcedLocalExitResult()
		}
		return result, stdinErr
	}
	if stdinSentInput.Load() {
		if _, _, controlErr := waitForInteractiveExitOrControlErr(nil, &controlErrCh, executionStdinErrDrainTimeout); controlErr != nil {
			if isForceLocalExit() {
				return forcedLocalExitResult()
			}
			return result, controlErr
		}
	}

	if !haveExitCode {
		if fetchedExitCode, ok := getFinalExecutionExitCode(rootCtx, client, sandboxID, executionID); ok {
			exitCode = fetchedExitCode
			haveExitCode = true
		}
	}

	if !haveExitCode {
		return result, fmt.Errorf("%s stream ended without exit status", label)
	}
	if exitCode != 0 {
		return result, exitCodeError{code: exitCode}
	}
	return result, nil
}
