package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

func runExecutionListWithCapture(cmd ExecutionListCommand, ctx runtimeContext) execOutcome {
	return runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, nil, ctx)
}

func TestExecutionListIntegrationShowsActiveExecutionsByDefault(t *testing.T) {
	blockExecution := make(chan struct{})
	adapter := &integrationAdapter{
		runFn: func(_ context.Context, req backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			<-blockExecution
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
			}, nil
		},
	}

	host, svc := startIntegrationServer(t, adapter)
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	createResp, err := client.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sleep", "1"},
		Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createResp.GetExecution().GetExecutionId()

	outcome := runExecutionListWithCapture(ExecutionListCommand{
		clientFlags: clientFlags{Host: host},
	}, runtimeContext{CWD: t.TempDir()})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecutionListCommand.Run returned error: %v", outcome.err)
	}
	assertContainsAll(t, outcome.stdout, "ID", executionID, sandboxID)

	close(blockExecution)
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}
}

func TestExecutionListIntegrationIncludesFinishedExecutionsWithAll(t *testing.T) {
	adapter := &integrationAdapter{
		runFn: func(_ context.Context, req backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
			}, nil
		},
	}

	host, svc := startIntegrationServer(t, adapter)
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	createResp, err := client.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "ok"},
		Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createResp.GetExecution().GetExecutionId()
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}

	defaultOutcome := runExecutionListWithCapture(ExecutionListCommand{
		clientFlags: clientFlags{Host: host},
	}, runtimeContext{CWD: t.TempDir()})
	if defaultOutcome.cause != nil {
		t.Fatalf("capture failure: %v", defaultOutcome.cause)
	}
	if defaultOutcome.err != nil {
		t.Fatalf("ExecutionListCommand.Run returned error: %v", defaultOutcome.err)
	}
	if !strings.Contains(defaultOutcome.stdout, "no active executions") {
		t.Fatalf("expected default list to hide finished executions, got %q", defaultOutcome.stdout)
	}

	allOutcome := runExecutionListWithCapture(ExecutionListCommand{
		clientFlags: clientFlags{Host: host},
		All:         true,
	}, runtimeContext{CWD: t.TempDir()})
	if allOutcome.cause != nil {
		t.Fatalf("capture failure: %v", allOutcome.cause)
	}
	if allOutcome.err != nil {
		t.Fatalf("ExecutionListCommand.Run with --all returned error: %v", allOutcome.err)
	}
	assertContainsAll(t, allOutcome.stdout, "ID", executionID, sandboxID, "succeeded")
}
