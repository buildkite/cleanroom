package cli

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type ConsoleCommand struct {
	clientFlags
	Chdir   string `short:"c" help:"Change to this directory before running commands"`
	Backend string `help:"Execution backend (defaults to runtime config or host default)"`
	In      string `name:"in" aliases:"sandbox-id" help:"Run in an existing sandbox ID instead of creating a new one"`
	From    string `name:"from" help:"Create the sandbox from an existing snapshot ID"`
	Image   string `help:"Override sandbox image ref for newly created sandboxes (tag, digest, or local Docker image)"`
	repositoryOverrideFlags
	workspaceCopyFlags
	Keep                bool     `help:"Keep a newly created sandbox after the console exits"`
	DangerouslyAllowAll bool     `name:"dangerously-allow-all" help:"Disable network egress filtering for a newly created sandbox"`
	Env                 []string `short:"e" name:"env" help:"Set guest environment variables; use KEY to inherit from the local environment or KEY=VALUE to set an explicit value"`
	PrintSandboxID      bool     `name:"print-sandbox-id" help:"Print resolved sandbox_id=<id> to stderr before attaching"`

	LaunchSeconds int64 `help:"VM boot/guest-agent readiness timeout in seconds"`

	Command   []string `arg:"" passthrough:"partial" optional:"" help:"Command to run with an interactive tty (default: sh)"`
	preAttach func(context.Context, *controlclient.Client, string) error
}

func (c *ConsoleCommand) Run(ctx *runtimeContext) (runErr error) {
	if err := validateExecutionSandboxArgs(c.Chdir, c.In, c.From, c.Keep, c.DangerouslyAllowAll, c.repositoryOverrideFlags, c.workspaceCopyFlags); err != nil {
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
	if err := validateWorkspaceCopyOutBeforeExecution(rootCtx, ctx, client, cwd, c.Chdir, c.In, c.LaunchSeconds, c.workspaceCopyFlags); err != nil {
		return err
	}

	target, err := resolveExecutionSandbox(rootCtx, client, ctx, cwd, host, c.Backend, c.In, c.From, c.Image, c.LaunchSeconds, c.DangerouslyAllowAll, c.repositoryOverrideFlags, c.workspaceCopyFlags)
	if err != nil {
		if strings.TrimSpace(c.From) != "" {
			err = explainSnapshotRuntimeDisabledError(err, ctx)
		}
		return err
	}
	sandboxID = target.SandboxID
	createdSandbox := target.CreatedSandbox
	repository := target.Repository
	if repository == nil && strings.TrimSpace(target.WorkspaceRoot) != "" {
		command = repositorycheckout.WrapCommandInWorkdir(command, workspaceWorkdirCheckout(target.WorkspaceRoot))
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
		// Non-zero exits should leave stderr focused on the streamed command output.
	}()
	if c.PrintSandboxID {
		if err := printSandboxID(); err != nil {
			return err
		}
	}
	detached := false
	preserveSandbox := false
	autoTerminateSandbox := createdSandbox && !c.Keep
	defer func() {
		if detached || preserveSandbox || sandboxID == "" || !autoTerminateSandbox {
			return
		}
		terminateSandboxBestEffort(rootCtx, client, sandboxID, 0, logger, "terminate sandbox after console failed")
	}()
	copyWorkspaceOutAfterRun := func(executionErr error) error {
		copyOutErr := copyWorkspaceOutAfterExecution(rootCtx, ctx, client, cwd, c.Chdir, sandboxID, c.LaunchSeconds, c.workspaceCopyFlags)
		if copyOutErr != nil && autoTerminateSandbox {
			preserveSandbox = true
			if err := printSandboxID(); err != nil {
				copyOutErr = errors.Join(copyOutErr, err)
			}
		}
		return joinExecutionAndWorkspaceCopyOutError(executionErr, copyOutErr)
	}
	if c.preAttach != nil {
		if err := c.preAttach(rootCtx, client, sandboxID); err != nil {
			return err
		}
	}

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
		return err
	}
	if shouldRunWorkspaceCopyOutAfterExecution(err) {
		return copyWorkspaceOutAfterRun(err)
	}
	return err
}
