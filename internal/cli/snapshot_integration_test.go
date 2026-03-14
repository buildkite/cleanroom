package cli

import (
	"encoding/json"
	"strings"
	"testing"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

func runSnapshotCreateWithCapture(cmd SnapshotCreateCommand, ctx runtimeContext) execOutcome {
	return runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, nil, ctx)
}

func runSnapshotGetWithCapture(cmd SnapshotGetCommand, ctx runtimeContext) execOutcome {
	return runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, nil, ctx)
}

func runSnapshotListWithCapture(cmd SnapshotListCommand, ctx runtimeContext) execOutcome {
	return runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, nil, ctx)
}

func runSnapshotDeleteWithCapture(cmd SnapshotDeleteCommand, ctx runtimeContext) execOutcome {
	return runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, nil, ctx)
}

func runSandboxRestoreWithCapture(cmd SandboxRestoreCommand, ctx runtimeContext) execOutcome {
	return runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, nil, ctx)
}

func TestSnapshotCommandsIntegrationLifecycle(t *testing.T) {
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
	if snapshotID == "" {
		t.Fatalf("expected snapshot id output, got %q", createOutcome.stdout)
	}
	if got, want := adapter.createSnapshotReq.SandboxID, sandboxID; got != want {
		t.Fatalf("unexpected snapshot sandbox id: got %q want %q", got, want)
	}

	getOutcome := runSnapshotGetWithCapture(SnapshotGetCommand{
		clientFlags: clientFlags{Host: host},
		SnapshotID:  snapshotID,
		JSON:        true,
	}, runtimeContext{CWD: cwd})
	if getOutcome.cause != nil {
		t.Fatalf("capture failure: %v", getOutcome.cause)
	}
	if getOutcome.err != nil {
		t.Fatalf("SnapshotGetCommand.Run returned error: %v", getOutcome.err)
	}

	var snapshot cleanroomv1.Snapshot
	if err := json.Unmarshal([]byte(getOutcome.stdout), &snapshot); err != nil {
		t.Fatalf("parse snapshot json: %v", err)
	}
	if got, want := snapshot.GetSnapshotId(), snapshotID; got != want {
		t.Fatalf("unexpected snapshot id: got %q want %q", got, want)
	}
	if got, want := snapshot.GetName(), "golden"; got != want {
		t.Fatalf("unexpected snapshot name: got %q want %q", got, want)
	}

	listOutcome := runSnapshotListWithCapture(SnapshotListCommand{
		clientFlags: clientFlags{Host: host},
	}, runtimeContext{CWD: cwd})
	if listOutcome.cause != nil {
		t.Fatalf("capture failure: %v", listOutcome.cause)
	}
	if listOutcome.err != nil {
		t.Fatalf("SnapshotListCommand.Run returned error: %v", listOutcome.err)
	}
	if !strings.Contains(listOutcome.stdout, snapshotID) || !strings.Contains(listOutcome.stdout, "golden") {
		t.Fatalf("expected snapshot list output to include snapshot row, got %q", listOutcome.stdout)
	}

	deleteOutcome := runSnapshotDeleteWithCapture(SnapshotDeleteCommand{
		clientFlags: clientFlags{Host: host},
		SnapshotID:  snapshotID,
	}, runtimeContext{CWD: cwd})
	if deleteOutcome.cause != nil {
		t.Fatalf("capture failure: %v", deleteOutcome.cause)
	}
	if deleteOutcome.err != nil {
		t.Fatalf("SnapshotDeleteCommand.Run returned error: %v", deleteOutcome.err)
	}
	if !strings.Contains(deleteOutcome.stdout, "snapshot deleted") {
		t.Fatalf("expected delete output, got %q", deleteOutcome.stdout)
	}
	if got, want := adapter.deleteSnapshotReq.SnapshotID, snapshotID; got != want {
		t.Fatalf("unexpected deleted snapshot id: got %q want %q", got, want)
	}

	emptyListOutcome := runSnapshotListWithCapture(SnapshotListCommand{
		clientFlags: clientFlags{Host: host},
	}, runtimeContext{CWD: cwd})
	if emptyListOutcome.cause != nil {
		t.Fatalf("capture failure: %v", emptyListOutcome.cause)
	}
	if emptyListOutcome.err != nil {
		t.Fatalf("SnapshotListCommand.Run after delete returned error: %v", emptyListOutcome.err)
	}
	if got := strings.TrimSpace(emptyListOutcome.stdout); got != "no snapshots" {
		t.Fatalf("unexpected empty snapshot list output: %q", got)
	}
}

func TestSandboxSnapshotIntegrationCreateFromSnapshotAndRestore(t *testing.T) {
	adapter := &snapshotIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()

	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	createSnapshotOutcome := runSnapshotCreateWithCapture(SnapshotCreateCommand{
		clientFlags: clientFlags{Host: host},
		SandboxID:   sandboxID,
		Name:        "golden",
	}, runtimeContext{CWD: cwd})
	if createSnapshotOutcome.cause != nil {
		t.Fatalf("capture failure: %v", createSnapshotOutcome.cause)
	}
	if createSnapshotOutcome.err != nil {
		t.Fatalf("SnapshotCreateCommand.Run returned error: %v", createSnapshotOutcome.err)
	}
	snapshotID := strings.TrimSpace(createSnapshotOutcome.stdout)
	if snapshotID == "" {
		t.Fatalf("expected snapshot id output, got %q", createSnapshotOutcome.stdout)
	}

	createOutcome := runSandboxCreateWithCapture(SandboxCreateCommand{
		clientFlags:  clientFlags{Host: host},
		FromSnapshot: snapshotID,
	}, runtimeContext{
		CWD:    cwd,
		Loader: failingLoader{},
	})
	if createOutcome.cause != nil {
		t.Fatalf("capture failure: %v", createOutcome.cause)
	}
	if createOutcome.err != nil {
		t.Fatalf("SandboxCreateCommand.Run returned error: %v", createOutcome.err)
	}

	forkSandboxID := strings.TrimSpace(createOutcome.stdout)
	if forkSandboxID == "" {
		t.Fatalf("expected sandbox id output, got %q", createOutcome.stdout)
	}
	requireSandboxStatus(t, client, forkSandboxID, cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY)
	if got, want := adapter.provisionFromSnapshotReq.SnapshotID, snapshotID; got != want {
		t.Fatalf("unexpected provision snapshot id: got %q want %q", got, want)
	}
	if got, want := adapter.provisionFromSnapshotReq.StorageRef, "/snapshots/"+snapshotID+".ext4"; got != want {
		t.Fatalf("unexpected provision storage ref: got %q want %q", got, want)
	}
	if adapter.provisionFromSnapshotReq.Policy == nil {
		t.Fatal("expected compiled policy on provision-from-snapshot request")
	}

	restoreOutcome := runSandboxRestoreWithCapture(SandboxRestoreCommand{
		clientFlags: clientFlags{Host: host},
		SandboxID:   sandboxID,
		SnapshotID:  snapshotID,
	}, runtimeContext{CWD: cwd})
	if restoreOutcome.cause != nil {
		t.Fatalf("capture failure: %v", restoreOutcome.cause)
	}
	if restoreOutcome.err != nil {
		t.Fatalf("SandboxRestoreCommand.Run returned error: %v", restoreOutcome.err)
	}
	if !strings.Contains(restoreOutcome.stdout, "sandbox restored from snapshot") {
		t.Fatalf("expected restore output, got %q", restoreOutcome.stdout)
	}
	if got, want := adapter.restoreReq.SandboxID, sandboxID; got != want {
		t.Fatalf("unexpected restore sandbox id: got %q want %q", got, want)
	}
	if got, want := adapter.restoreReq.SnapshotID, snapshotID; got != want {
		t.Fatalf("unexpected restore snapshot id: got %q want %q", got, want)
	}
	if adapter.restoreReq.Policy == nil {
		t.Fatal("expected compiled policy on restore request")
	}
}

func TestSnapshotDeleteIntegrationAllowsDeleteAfterSnapshotBackedCreate(t *testing.T) {
	adapter := &snapshotIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()

	client := mustNewControlClient(t, host)
	sourceSandboxID := mustCreateSandbox(t, client)

	createSnapshotOutcome := runSnapshotCreateWithCapture(SnapshotCreateCommand{
		clientFlags: clientFlags{Host: host},
		SandboxID:   sourceSandboxID,
		Name:        "golden",
	}, runtimeContext{CWD: cwd})
	if createSnapshotOutcome.cause != nil {
		t.Fatalf("capture failure: %v", createSnapshotOutcome.cause)
	}
	if createSnapshotOutcome.err != nil {
		t.Fatalf("SnapshotCreateCommand.Run returned error: %v", createSnapshotOutcome.err)
	}
	snapshotID := strings.TrimSpace(createSnapshotOutcome.stdout)

	createOutcome := runSandboxCreateWithCapture(SandboxCreateCommand{
		clientFlags:  clientFlags{Host: host},
		FromSnapshot: snapshotID,
	}, runtimeContext{
		CWD:    cwd,
		Loader: failingLoader{},
	})
	if createOutcome.cause != nil {
		t.Fatalf("capture failure: %v", createOutcome.cause)
	}
	if createOutcome.err != nil {
		t.Fatalf("SandboxCreateCommand.Run returned error: %v", createOutcome.err)
	}
	if got := strings.TrimSpace(createOutcome.stdout); got == "" {
		t.Fatal("expected forked sandbox id output")
	}

	deleteOutcome := runSnapshotDeleteWithCapture(SnapshotDeleteCommand{
		clientFlags: clientFlags{Host: host},
		SnapshotID:  snapshotID,
	}, runtimeContext{CWD: cwd})
	if deleteOutcome.cause != nil {
		t.Fatalf("capture failure: %v", deleteOutcome.cause)
	}
	if deleteOutcome.err != nil {
		t.Fatalf("SnapshotDeleteCommand.Run returned error: %v", deleteOutcome.err)
	}
	if !strings.Contains(deleteOutcome.stdout, "snapshot deleted") {
		t.Fatalf("expected delete output, got %q", deleteOutcome.stdout)
	}
	if got, want := adapter.deleteSnapshotReq.SnapshotID, snapshotID; got != want {
		t.Fatalf("unexpected deleted snapshot id: got %q want %q", got, want)
	}
}
