package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type ExecutionCommand struct {
	List    ExecutionListCommand    `name:"ls" aliases:"list" cmd:"" help:"List known executions"`
	Inspect ExecutionInspectCommand `name:"inspect" aliases:"show" cmd:"" help:"Inspect an execution and its diagnostics"`
}

type ExecutionListCommand struct {
	clientFlags
	SandboxID string `name:"sandbox-id" help:"Only list executions for this sandbox"`
	All       bool   `help:"Include finished executions"`
	JSON      bool   `help:"Print executions as JSON"`
}

type ExecutionInspectCommand struct {
	clientFlags
	SandboxID   string `name:"sandbox-id" help:"Sandbox ID that owns or scopes the execution"`
	ExecutionID string `arg:"" optional:"" help:"Execution ID to inspect"`
	Last        bool   `help:"Inspect the most recent execution globally or within --sandbox-id"`
	JSON        bool   `help:"Print execution inspection as JSON"`
}

func (c *ExecutionListCommand) Run(ctx *runtimeContext) error {
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}

	resp, err := client.ListExecutions(context.Background(), &cleanroomv1.ListExecutionsRequest{
		SandboxId: strings.TrimSpace(c.SandboxID),
		All:       c.All,
	})
	if err != nil {
		return fmt.Errorf("list executions: %w", err)
	}

	if c.JSON {
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp.GetExecutions())
	}

	executions := resp.GetExecutions()
	if len(executions) == 0 {
		_, err := fmt.Fprintln(ctx.Stdout, executionListEmptyMessage(c.All, strings.TrimSpace(c.SandboxID)))
		return err
	}

	tw := tabwriter.NewWriter(ctx.Stdout, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tSTATUS\tKIND\tSANDBOX\tSTARTED\tFINISHED"); err != nil {
		return err
	}
	for _, execution := range executions {
		if execution == nil {
			continue
		}
		started := ""
		if startedAt := execution.GetStartedAt(); startedAt != nil {
			started = startedAt.AsTime().Format(timeFormat)
		}
		finished := ""
		if finishedAt := execution.GetFinishedAt(); finishedAt != nil {
			finished = finishedAt.AsTime().Format(timeFormat)
		}
		if _, err := fmt.Fprintf(
			tw,
			"%s\t%s\t%s\t%s\t%s\t%s\n",
			execution.GetExecutionId(),
			executionStatusString(execution.GetStatus()),
			executionKindString(execution.GetKind()),
			execution.GetSandboxId(),
			started,
			finished,
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func (c *ExecutionInspectCommand) Run(ctx *runtimeContext) error {
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}

	sandboxID := strings.TrimSpace(c.SandboxID)
	executionID := strings.TrimSpace(c.ExecutionID)
	if c.Last {
		if executionID != "" {
			return errors.New("choose either <execution-id> or --last")
		}
		listResp, err := client.ListExecutions(context.Background(), &cleanroomv1.ListExecutionsRequest{
			SandboxId: sandboxID,
			All:       true,
		})
		if err != nil {
			return fmt.Errorf("list executions: %w", err)
		}
		for _, execution := range listResp.GetExecutions() {
			if execution == nil {
				continue
			}
			executionID = strings.TrimSpace(execution.GetExecutionId())
			selectedSandboxID := strings.TrimSpace(execution.GetSandboxId())
			if executionID != "" {
				if sandboxID == "" && selectedSandboxID != "" {
					sandboxID = selectedSandboxID
				}
				break
			}
		}
		if executionID == "" {
			if sandboxID != "" {
				return fmt.Errorf("sandbox %q has no recorded executions", sandboxID)
			}
			return errors.New("no recorded executions")
		}
	} else {
		if executionID == "" {
			return errors.New("missing <execution-id> or use --last")
		}
	}

	resp, err := client.InspectExecution(context.Background(), &cleanroomv1.InspectExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
	})
	if err != nil {
		return fmt.Errorf("inspect execution: %w", err)
	}
	resolvedSandboxID := sandboxID
	if execution := resp.GetExecution(); execution != nil && strings.TrimSpace(resolvedSandboxID) == "" {
		resolvedSandboxID = execution.GetSandboxId()
	}
	if strings.TrimSpace(resp.GetTraceUrl()) == "" {
		traceURL, err := runtimeconfig.RenderTraceURL(ctx.Config.Observability, resp.GetTraceId(), executionID, resolvedSandboxID)
		if err == nil {
			resp.TraceUrl = traceURL
		}
	}

	if c.JSON {
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	execution := resp.GetExecution()
	if execution == nil {
		return errors.New("inspect execution returned no execution")
	}

	if _, err := fmt.Fprintf(ctx.Stdout, "execution: %s\n", execution.GetExecutionId()); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(ctx.Stdout, "sandbox: %s\n", execution.GetSandboxId()); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(ctx.Stdout, "status: %s\n", executionStatusString(execution.GetStatus())); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(ctx.Stdout, "kind: %s\n", executionKindString(execution.GetKind())); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(ctx.Stdout, "exit_code: %d\n", execution.GetExitCode()); err != nil {
		return err
	}
	if started := execution.GetStartedAt(); started != nil {
		if _, err := fmt.Fprintf(ctx.Stdout, "started_at: %s\n", started.AsTime().Format(timeFormat)); err != nil {
			return err
		}
	}
	if finished := execution.GetFinishedAt(); finished != nil {
		if _, err := fmt.Fprintf(ctx.Stdout, "finished_at: %s\n", finished.AsTime().Format(timeFormat)); err != nil {
			return err
		}
	}
	if msg := strings.TrimSpace(resp.GetMessage()); msg != "" {
		if _, err := fmt.Fprintf(ctx.Stdout, "message: %s\n", msg); err != nil {
			return err
		}
	}
	if dir := strings.TrimSpace(resp.GetArtifactsDir()); dir != "" {
		if _, err := fmt.Fprintf(ctx.Stdout, "artifacts_dir: %s\n", dir); err != nil {
			return err
		}
	}
	if planPath := strings.TrimSpace(resp.GetPlanPath()); planPath != "" {
		if _, err := fmt.Fprintf(ctx.Stdout, "plan_path: %s\n", planPath); err != nil {
			return err
		}
	}
	if imageRef := strings.TrimSpace(resp.GetImageRef()); imageRef != "" {
		if _, err := fmt.Fprintf(ctx.Stdout, "image_ref: %s\n", imageRef); err != nil {
			return err
		}
	}
	if imageDigest := strings.TrimSpace(resp.GetImageDigest()); imageDigest != "" {
		if _, err := fmt.Fprintf(ctx.Stdout, "image_digest: %s\n", imageDigest); err != nil {
			return err
		}
	}
	if traceID := strings.TrimSpace(resp.GetTraceId()); traceID != "" {
		if _, err := fmt.Fprintf(ctx.Stdout, "trace_id: %s\n", traceID); err != nil {
			return err
		}
	}
	if traceURL := strings.TrimSpace(resp.GetTraceUrl()); traceURL != "" {
		if _, err := fmt.Fprintf(ctx.Stdout, "trace_url: %s\n", traceURL); err != nil {
			return err
		}
	}

	if stderr := strings.TrimSpace(resp.GetStderr()); stderr != "" {
		if _, err := fmt.Fprint(ctx.Stdout, "\nstderr:\n"+stderr+"\n"); err != nil {
			return err
		}
	}
	if stdout := strings.TrimSpace(resp.GetStdout()); stdout != "" {
		if _, err := fmt.Fprint(ctx.Stdout, "\nstdout:\n"+stdout+"\n"); err != nil {
			return err
		}
	}
	if obs := resp.GetObservability(); obs != nil {
		formatted, err := json.MarshalIndent(obs.AsMap(), "", "  ")
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(ctx.Stdout, "\nobservability:\n%s\n", formatted); err != nil {
			return err
		}
	}
	return nil
}

const timeFormat = "2006-01-02T15:04:05Z07:00"

func executionListEmptyMessage(includeFinished bool, sandboxID string) string {
	scope := ""
	if strings.TrimSpace(sandboxID) != "" {
		scope = " for sandbox " + strings.TrimSpace(sandboxID)
	}
	if includeFinished {
		return "no recorded executions" + scope
	}
	return "no active executions" + scope
}

func executionStatusString(status cleanroomv1.ExecutionStatus) string {
	switch status {
	case cleanroomv1.ExecutionStatus_EXECUTION_STATUS_QUEUED:
		return "queued"
	case cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING:
		return "running"
	case cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED:
		return "succeeded"
	case cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED:
		return "failed"
	case cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED:
		return "canceled"
	case cleanroomv1.ExecutionStatus_EXECUTION_STATUS_TIMED_OUT:
		return "timed_out"
	default:
		return "unknown"
	}
}

func executionKindString(kind cleanroomv1.ExecutionKind) string {
	switch kind {
	case cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH:
		return "batch"
	case cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE:
		return "interactive"
	default:
		return "unknown"
	}
}
