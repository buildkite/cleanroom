package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

func runSandboxInspectWithCapture(cmd SandboxInspectCommand, ctx runtimeContext) execOutcome {
	return runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, nil, ctx)
}

func TestSandboxInspectIntegrationShowsExecutionPointers(t *testing.T) {
	host, svc := startIntegrationServer(t, &integrationAdapter{})
	cwd := t.TempDir()

	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	createExecutionResp, err := client.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "ok"},
		Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}

	outcome := runSandboxInspectWithCapture(SandboxInspectCommand{
		clientFlags: clientFlags{Host: host},
		SandboxID:   sandboxID,
	}, runtimeContext{CWD: cwd})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("SandboxInspectCommand.Run returned error: %v", outcome.err)
	}
	if !strings.Contains(outcome.stdout, "sandbox: "+sandboxID) {
		t.Fatalf("expected sandbox id in output, got %q", outcome.stdout)
	}
	if !strings.Contains(outcome.stdout, "status: ready") {
		t.Fatalf("expected sandbox status in output, got %q", outcome.stdout)
	}
	if !strings.Contains(outcome.stdout, "last_execution_id: "+executionID) {
		t.Fatalf("expected last execution id in output, got %q", outcome.stdout)
	}
	if !strings.Contains(outcome.stdout, "inspect_last_execution: cleanroom execution inspect --sandbox-id "+sandboxID+" --last") {
		t.Fatalf("expected inspect hint in output, got %q", outcome.stdout)
	}
}

func TestSandboxInspectIntegrationJSON(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	cwd := t.TempDir()

	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	outcome := runSandboxInspectWithCapture(SandboxInspectCommand{
		clientFlags: clientFlags{Host: host},
		SandboxID:   sandboxID,
		JSON:        true,
	}, runtimeContext{CWD: cwd})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("SandboxInspectCommand.Run returned error: %v", outcome.err)
	}

	var sandbox cleanroomv1.Sandbox
	if err := json.Unmarshal([]byte(outcome.stdout), &sandbox); err != nil {
		t.Fatalf("parse sandbox json: %v", err)
	}
	if got, want := sandbox.GetSandboxId(), sandboxID; got != want {
		t.Fatalf("unexpected sandbox id: got %q want %q", got, want)
	}
}
