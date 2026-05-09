package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
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
	if !strings.Contains(outcome.stdout, "inspect_last_execution: cleanroom execution inspect "+executionID) {
		t.Fatalf("expected inspect hint in output, got %q", outcome.stdout)
	}
}

func TestSandboxInspectIntegrationShowsEffectiveResources(t *testing.T) {
	host, _ := startIntegrationServerWithConfig(t, &integrationAdapter{}, runtimeconfig.Config{
		DefaultBackend: "firecracker",
		Backends: runtimeconfig.Backends{
			Firecracker: runtimeconfig.FirecrackerConfig{
				VCPUs:     2,
				MemoryMiB: 1024,
			},
		},
	})
	cwd := t.TempDir()

	client := mustNewControlClient(t, host)
	compiled, _, err := integrationLoader{}.LoadAndCompile(cwd)
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	policyProto := compiled.ToProto()
	policyProto.Resources = &cleanroomv1.PolicyResources{
		Vcpus:       4,
		MemoryBytes: (3 << 30) + 1,
		DiskBytes:   16 << 30,
	}
	createResp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: policyProto})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

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
	assertContainsAll(t, outcome.stdout,
		"effective_resources:",
		"  vcpus: 4",
		"  memory: 3073 MiB",
		"  disk: 16.0 GiB",
	)
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

func TestSandboxInspectIntegrationSupportsLast(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	cwd := t.TempDir()

	client := mustNewControlClient(t, host)
	_ = mustCreateSandbox(t, client)
	latestSandboxID := mustCreateSandbox(t, client)

	outcome := runSandboxInspectWithCapture(SandboxInspectCommand{
		clientFlags: clientFlags{Host: host},
		Last:        true,
	}, runtimeContext{CWD: cwd})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("SandboxInspectCommand.Run returned error: %v", outcome.err)
	}
	if !strings.Contains(outcome.stdout, "sandbox: "+latestSandboxID) {
		t.Fatalf("expected latest sandbox id in output, got %q", outcome.stdout)
	}
}

func TestSandboxInspectIntegrationShowsProvenance(t *testing.T) {
	host, _ := startIntegrationServer(t, &snapshotIntegrationAdapter{})
	cwd := t.TempDir()

	client := mustNewControlClient(t, host)
	sourceSandboxID := mustCreateSandbox(t, client)
	snapshotResp, err := client.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: sourceSandboxID,
		Name:      "inspect-provenance",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}
	snapshotID := snapshotResp.GetSnapshot().GetSnapshotId()
	createResp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{SnapshotId: snapshotID},
	})
	if err != nil {
		t.Fatalf("CreateSandbox from snapshot returned error: %v", err)
	}
	provenanceSandboxID := createResp.GetSandbox().GetSandboxId()

	outcome := runSandboxInspectWithCapture(SandboxInspectCommand{
		clientFlags: clientFlags{Host: host},
		SandboxID:   provenanceSandboxID,
	}, runtimeContext{CWD: cwd})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("SandboxInspectCommand.Run returned error: %v", outcome.err)
	}
	assertContainsAll(t, outcome.stdout,
		"source_kind: snapshot",
		"source_id: "+snapshotID,
		"backing_snapshot_id: "+snapshotID,
	)
}
