package cli

import (
	"context"
	"errors"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"os"
	"strings"
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

	Command []string `arg:"" passthrough:"partial" optional:"" help:"Command to run with an interactive tty (default: sh)"`
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
		attribute.String(observability.AttrBackendRequested, strings.TrimSpace(c.Backend)),
		attribute.Bool(observability.AttrKeepSandbox, c.Keep),
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
		observability.SpanConsole,
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

	target, err := resolveExecutionSandbox(rootCtx, client, ctx, cwd, host, c.Backend, c.In, c.From, c.Image, c.LaunchSeconds, c.repositoryOverrideFlags, c.repositoryChangesetFlags)
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
	detached := false
	autoTerminateSandbox := createdSandbox && !c.Keep
	defer func() {
		if detached || sandboxID == "" || !autoTerminateSandbox {
			return
		}
		terminateSandboxBestEffort(rootCtx, client, sandboxID, 0, logger, "terminate sandbox after console failed")
	}()

	interactiveResult, err := runInteractiveExecution(
		rootCtx,
		ctx,
		client,
		logger,
		host,
		sandboxID,
		repository,
		command,
		executionEnv,
		c.LaunchSeconds,
		interactiveExecutionOptions{Label: "console"},
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
