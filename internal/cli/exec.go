package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

type ExecCommand struct {
	clientFlags
	Chdir          string `short:"c" help:"Change to this directory before running commands"`
	Backend        string `help:"Execution backend (defaults to runtime config or host default)"`
	In             string `name:"in" aliases:"sandbox-id" help:"Run in an existing sandbox ID instead of creating a new one"`
	From           string `name:"from" help:"Create the sandbox from an existing snapshot ID"`
	Image          string `help:"Override sandbox image ref for newly created sandboxes (tag, digest, or local Docker image)"`
	Keep           bool   `help:"Keep a newly created sandbox after the command completes"`
	NoStdin        bool   `short:"n" name:"no-stdin" aliases:"stdin-eof" help:"Close stdin immediately instead of attaching it"`
	PrintSandboxID bool   `name:"print-sandbox-id" help:"Print resolved sandbox_id=<id> to stderr before streaming output"`

	LaunchSeconds int64 `help:"VM boot/guest-agent readiness timeout in seconds"`

	Command []string `arg:"" passthrough:"partial" required:"" help:"Command to execute"`
}

func (e *ExecCommand) Run(ctx *runtimeContext) (runErr error) {
	logger, err := newLogger(e.LogLevel, "client")
	if err != nil {
		return err
	}

	host := e.resolvedHost(ctx.Config)
	client, err := e.connect(ctx)
	if err != nil {
		return err
	}
	cwd, err := resolveCWD(ctx.CWD, e.Chdir)
	if err != nil {
		return err
	}

	logger.Debug("sending execution request",
		"host", host,
		"backend", e.Backend,
		"sandbox_id", strings.TrimSpace(e.In),
		"command_argc", len(e.Command),
	)
	persistentRepositoryBackend := backendSupportsRepositoryPersistence(ctx, host, e.Backend)
	repository, err := maybeResolveRepositoryCheckout(cwd, ctx.Loader, strings.TrimSpace(e.In), strings.TrimSpace(e.From), !persistentRepositoryBackend)
	if err != nil {
		return err
	}
	warnDirtyRepositoryCheckout(repository)
	inlineRepositoryBootstrap := shouldInlineRepositoryBootstrap(ctx, host, e.Backend, repository)
	repositoryForCreate := repository
	if inlineRepositoryBootstrap {
		repositoryForCreate = nil
	}
	if strings.TrimSpace(e.In) != "" && e.Keep {
		return errors.New("--keep cannot be used with --in")
	}
	sandboxID, createdSandbox, err := ensureSandboxID(client, ctx.Loader, cwd, host, e.Backend, strings.TrimSpace(e.In), strings.TrimSpace(e.From), e.Image, e.LaunchSeconds, repositoryForCreate)
	if err != nil {
		if strings.TrimSpace(e.From) != "" {
			err = explainSnapshotRuntimeDisabledError(err, ctx)
		}
		return err
	}
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
	defer func() {
		if !createdSandbox || !e.Keep {
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
	if e.PrintSandboxID {
		if err := printSandboxID(); err != nil {
			return err
		}
	}
	detached := false
	autoTerminateSandbox := createdSandbox && !e.Keep
	defer func() {
		if detached || !autoTerminateSandbox || sandboxID == "" {
			return
		}
		terminateSandboxBestEffort(client, sandboxID, 0, logger, "terminate sandbox after exec failed")
	}()

	createExecutionResp, err := client.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId:          sandboxID,
		Command:            repositoryExecutionCommand(e.Command, repository, inlineRepositoryBootstrap),
		Kind:               cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
		RepositoryCheckout: repositoryExecutionCheckout(repository, inlineRepositoryBootstrap),
		Options: &cleanroomv1.ExecutionOptions{
			LaunchSeconds: e.LaunchSeconds,
		},
	})
	if err != nil {
		return fmt.Errorf("create execution: %w", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	logger.Debug("execution started", "sandbox_id", sandboxID, "execution_id", executionID)

	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	stream, err := client.StreamExecution(streamCtx, &cleanroomv1.StreamExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Follow:      true,
	})
	if err != nil {
		return fmt.Errorf("stream execution: %w", err)
	}

	stdinErrCh := startExecutionStdinForwarder(client, sandboxID, executionID, e.NoStdin)
	stdinErrCh = monitorExecutionStdinErr(streamCtx, streamCancel, stdinErrCh)

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
		case *cleanroomv1.ExecutionStreamEvent_Warning:
			if err := writeExecutionWarning(os.Stderr, payload.Warning); err != nil {
				return err
			}
		case *cleanroomv1.ExecutionStreamEvent_Exit:
			exitCode = int(payload.Exit.GetExitCode())
			haveExitCode = true
		}
		if stdinErr := pollExecutionStdinErr(stdinErrCh); stdinErr != nil {
			return stdinErr
		}
	}

	streamErr := stream.Err()
	select {
	case <-secondInterrupt:
		detached = true
		if autoTerminateSandbox {
			terminateSandboxBestEffort(client, sandboxID, sandboxTerminateTimeout, logger, "terminate sandbox after detach failed")
		}
		return exitCodeError{code: 130}
	default:
	}

	if streamErr != nil && !isCanceledStreamErr(streamErr) {
		return fmt.Errorf("stream execution: %w", streamErr)
	}

	if stdinErr := pollExecutionStdinErr(stdinErrCh); stdinErr != nil {
		return stdinErr
	}

	if !haveExitCode {
		if fetchedExitCode, ok := getFinalExecutionExitCode(client, sandboxID, executionID); ok {
			exitCode = fetchedExitCode
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
