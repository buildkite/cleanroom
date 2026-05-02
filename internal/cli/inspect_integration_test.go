package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

func runInspectWithCapture(cmd InspectCommand, ctx runtimeContext) execOutcome {
	return runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, nil, ctx)
}

func TestInspectIntegrationDispatchesSandboxID(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	cwd := t.TempDir()

	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	outcome := runInspectWithCapture(InspectCommand{
		clientFlags: clientFlags{Host: host},
		ID:          sandboxID,
	}, runtimeContext{CWD: cwd})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("InspectCommand.Run returned error: %v", outcome.err)
	}
	assertContainsAll(t, outcome.stdout, "sandbox: "+sandboxID, "status: ready")
}

func TestInspectIntegrationDispatchesExecutionID(t *testing.T) {
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

	outcome := runInspectWithCapture(InspectCommand{
		clientFlags: clientFlags{Host: host},
		ID:          executionID,
	}, runtimeContext{CWD: cwd})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("InspectCommand.Run returned error: %v", outcome.err)
	}
	assertContainsAll(t, outcome.stdout, "execution: "+executionID, "sandbox: "+sandboxID, "status: succeeded")
}

func TestInspectIntegrationDispatchesSnapshotID(t *testing.T) {
	adapter := &snapshotIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()

	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	createOutcome := runSnapshotCreateWithCapture(SnapshotCreateCommand{
		clientFlags: clientFlags{Host: host},
		SandboxID:   sandboxID,
		Name:        "golden",
	}, runtimeContext{CWD: cwd})
	if createOutcome.cause != nil {
		t.Fatalf("capture failure: %v", createOutcome.cause)
	}
	if createOutcome.err != nil {
		t.Fatalf("SnapshotCreateCommand.Run returned error: %v", createOutcome.err)
	}
	snapshotID := strings.TrimSpace(createOutcome.stdout)

	outcome := runInspectWithCapture(InspectCommand{
		clientFlags: clientFlags{Host: host},
		ID:          snapshotID,
		JSON:        true,
	}, runtimeContext{CWD: cwd})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("InspectCommand.Run returned error: %v", outcome.err)
	}

	var snapshot cleanroomv1.Snapshot
	if err := json.Unmarshal([]byte(outcome.stdout), &snapshot); err != nil {
		t.Fatalf("parse snapshot json: %v", err)
	}
	if got, want := snapshot.GetSnapshotId(), snapshotID; got != want {
		t.Fatalf("unexpected snapshot id: got %q want %q", got, want)
	}
}

func TestInspectCommandRejectsUnknownIDKind(t *testing.T) {
	outcome := runInspectWithCapture(InspectCommand{
		ID: "weird_123",
	}, runtimeContext{CWD: t.TempDir()})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(outcome.err.Error(), "unrecognized inspect target") {
		t.Fatalf("unexpected validation error: %v", outcome.err)
	}
}

func TestInspectTargetKindRecognizesSandboxFallbackID(t *testing.T) {
	kind, err := inspectTargetKind("cr-123")
	if err != nil {
		t.Fatalf("inspectTargetKind returned error: %v", err)
	}
	if got, want := kind, "sandbox"; got != want {
		t.Fatalf("unexpected target kind: got %q want %q", got, want)
	}
}
