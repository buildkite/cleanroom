package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type ExecCommand struct {
	clientFlags
	Chdir   string `short:"c" help:"Change to this directory before running commands"`
	Backend string `help:"Execution backend (defaults to runtime config or host default)"`
	In      string `name:"in" aliases:"sandbox-id" help:"Run in an existing sandbox ID instead of creating a new one"`
	From    string `name:"from" help:"Create the sandbox from an existing snapshot ID"`
	Image   string `help:"Override sandbox image ref for newly created sandboxes (tag, digest, or local Docker image)"`
	repositoryOverrideFlags
	workspaceCopyFlags
	Keep                bool     `help:"Keep a newly created sandbox after the command completes"`
	DangerouslyAllowAll bool     `name:"dangerously-allow-all" help:"Disable network egress filtering for a newly created sandbox"`
	Env                 []string `short:"e" name:"env" help:"Set guest environment variables; use KEY to inherit from the local environment or KEY=VALUE to set an explicit value"`
	TTY                 bool     `name:"tty" help:"Allocate a tty and attach through the interactive transport; stdout and stderr merge into a single stream"`
	NoStdin             bool     `short:"n" name:"no-stdin" aliases:"stdin-eof" help:"Close stdin immediately instead of attaching it"`
	PrintSandboxID      bool     `name:"print-sandbox-id" help:"Print resolved sandbox_id=<id> to stderr before streaming output"`
	PrintTraceID        bool     `name:"print-trace-id" help:"Print trace_id=<id> to stderr after a successful execution when available"`

	LaunchSeconds int64 `help:"VM boot/guest-agent readiness timeout in seconds"`

	Command []string `arg:"" passthrough:"partial" required:"" help:"Command to execute"`
}

func (e *ExecCommand) Run(ctx *runtimeContext) (runErr error) {
	if err := validateExecutionSandboxArgs(e.Chdir, e.In, e.From, e.Keep, e.DangerouslyAllowAll, e.repositoryOverrideFlags, e.workspaceCopyFlags); err != nil {
		return err
	}
	if e.TTY && e.NoStdin {
		return errors.New("--no-stdin cannot be used with --tty")
	}

	sandboxID := ""
	executionID := ""
	commandArgs := executionCommandArgs(e.Command)
	rootAttrs := []attribute.KeyValue{
		attribute.String(observability.AttrBackendRequested, strings.TrimSpace(e.Backend)),
		attribute.Bool(observability.AttrKeepSandbox, e.Keep),
		attribute.Bool(observability.AttrStdinDisabled, e.NoStdin),
		attribute.Int(observability.AttrCommandArgc, len(commandArgs)),
	}
	if commandName := executionCommandName(commandArgs); commandName != "" {
		rootAttrs = append(rootAttrs,
			attribute.String(observability.AttrCommandName, commandName),
			attribute.String(observability.AttrCommandSummary, executionCommandSummary(commandArgs)),
		)
	}
	rootCtx, rootSpan := ctx.Observability.Tracer("github.com/buildkite/cleanroom/internal/cli").Start(
		context.Background(),
		observability.SpanExec,
		trace.WithAttributes(rootAttrs...),
	)
	traceID := traceIDFromContext(rootCtx)
	defer func() {
		if sandboxID != "" {
			rootSpan.SetAttributes(attribute.String(observability.AttrSandboxID, sandboxID))
		}
		if executionID != "" {
			rootSpan.SetAttributes(attribute.String(observability.AttrExecutionID, executionID))
		}
		if runErr != nil {
			rootSpan.RecordError(runErr)
			rootSpan.SetStatus(codes.Error, runErr.Error())
		} else {
			rootSpan.SetStatus(codes.Ok, "")
		}
		rootSpan.End()
	}()

	logger := newClientLogger()

	host := e.resolvedHost(ctx.Config)
	client, err := e.connect(ctx)
	if err != nil {
		return err
	}
	cwd, err := resolveCWD(ctx.CWD, e.Chdir)
	if err != nil {
		return err
	}
	executionEnv, err := resolveExecutionEnv(e.Env)
	if err != nil {
		return err
	}

	target, err := resolveExecutionSandbox(rootCtx, client, ctx, cwd, host, e.Backend, e.In, e.From, e.Image, e.LaunchSeconds, e.DangerouslyAllowAll, e.repositoryOverrideFlags, e.workspaceCopyFlags)
	if err != nil {
		if strings.TrimSpace(e.From) != "" {
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
	defer func() {
		if runErr != nil || !e.PrintTraceID {
			return
		}
		if err := printTraceID(); err != nil {
			if runErr == nil {
				runErr = err
				return
			}
			runErr = errors.Join(runErr, err)
		}
	}()
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
	defer func() {
		if runErr == nil {
			return
		}
		// Non-zero exits should leave stderr focused on the streamed command output.
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
		terminateSandboxBestEffort(rootCtx, client, sandboxID, 0, logger, "terminate sandbox after exec failed")
	}()
	if e.TTY {
		interactiveResult, err := runInteractiveExecution(
			rootCtx,
			ctx,
			client,
			logger,
			host,
			sandboxID,
			repository,
			e.Command,
			executionEnv,
			e.LaunchSeconds,
			interactiveExecutionOptions{
				Label:   "exec",
				NoStdin: e.NoStdin,
			},
		)
		if interactiveResult.ExecutionID != "" {
			executionID = interactiveResult.ExecutionID
		}
		if interactiveResult.ForcedLocalExit {
			detached = true
			if autoTerminateSandbox {
				terminateSandboxBestEffort(rootCtx, client, sandboxID, sandboxTerminateTimeout, logger, "terminate sandbox after detach failed")
			}
		}
		return err
	}

	createExecutionResp, err := client.CreateExecution(rootCtx, &cleanroomv1.CreateExecutionRequest{
		SandboxId:          sandboxID,
		Command:            repositorycheckout.NormalizeCommand(e.Command),
		Env:                executionEnv,
		Kind:               cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
		RepositoryCheckout: repositoryCheckoutProto(repository),
		Options: &cleanroomv1.ExecutionOptions{
			LaunchSeconds: e.LaunchSeconds,
		},
	})
	if err != nil {
		return fmt.Errorf("create execution: %w", err)
	}
	executionID = createExecutionResp.GetExecution().GetExecutionId()

	streamCtx, streamCancel := context.WithCancel(rootCtx)
	defer streamCancel()
	stream, err := client.StreamExecution(streamCtx, &cleanroomv1.StreamExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Follow:      true,
	})
	if err != nil {
		return fmt.Errorf("stream execution: %w", err)
	}

	stdinErrCh := startExecutionStdinForwarder(rootCtx, client, sandboxID, executionID, e.NoStdin, streamCancel)

	signalCh := newSignalChannel()
	notifySignals(signalCh, os.Interrupt, syscall.SIGTERM)
	defer stopSignals(signalCh)

	secondInterrupt := make(chan struct{}, 1)
	go func() {
		interrupts := 0
		for range signalCh {
			interrupts++
			if interrupts == 1 {
				cancelResp, cancelErr := client.CancelExecution(rootCtx, &cleanroomv1.CancelExecutionRequest{
					SandboxId:   sandboxID,
					ExecutionId: executionID,
					Signal:      2,
				})
				if cancelErr != nil && logger != nil {
					logger.Warn("cancel execution request failed", "sandbox_id", sandboxID, "execution_id", executionID, "error", cancelErr)
				} else if cancelResp != nil && !cancelResp.GetAccepted() && logger != nil {
					logger.Warn("cancel execution request was not accepted", "sandbox_id", sandboxID, "execution_id", executionID, "status", cancelResp.GetStatus().String())
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
			terminateSandboxBestEffort(rootCtx, client, sandboxID, sandboxTerminateTimeout, logger, "terminate sandbox after detach failed")
		}
		return exitCodeError{code: 130}
	default:
	}

	if streamErr != nil && !isCanceledStreamErr(streamErr) {
		return fmt.Errorf("stream execution: %w", streamErr)
	}

	if stdinErr := waitExecutionStdinErr(stdinErrCh, executionStdinErrDrainTimeout); stdinErr != nil {
		return stdinErr
	}

	if !haveExitCode {
		if fetchedExitCode, ok := getFinalExecutionExitCode(rootCtx, client, sandboxID, executionID); ok {
			exitCode = fetchedExitCode
			haveExitCode = true
		}
	}

	if !haveExitCode {
		return errors.New("execution stream ended without exit status")
	}
	if exitCode != 0 {
		return exitCodeError{code: exitCode}
	}
	return nil
}
