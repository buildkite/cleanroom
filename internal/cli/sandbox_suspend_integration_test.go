package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

type suspendIntegrationAdapter struct {
	integrationAdapter
	suspendCalls int
	resumeCalls  int
}

func (a *suspendIntegrationAdapter) SuspendSandbox(context.Context, string) error {
	a.suspendCalls++
	return nil
}

func (a *suspendIntegrationAdapter) ResumeSandbox(context.Context, string) error {
	a.resumeCalls++
	return nil
}

func runSandboxSuspendWithCapture(cmd SandboxSuspendCommand, ctx runtimeContext) execOutcome {
	return runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, nil, ctx)
}

func runSandboxResumeWithCapture(cmd SandboxResumeCommand, ctx runtimeContext) execOutcome {
	return runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, nil, ctx)
}

func TestSandboxSuspendResumeIntegration(t *testing.T) {
	adapter := &suspendIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()

	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	suspendOutcome := runSandboxSuspendWithCapture(SandboxSuspendCommand{
		clientFlags: clientFlags{Host: host},
		SandboxID:   sandboxID,
	}, runtimeContext{CWD: cwd})
	if suspendOutcome.cause != nil {
		t.Fatalf("capture failure: %v", suspendOutcome.cause)
	}
	if suspendOutcome.err != nil {
		t.Fatalf("SandboxSuspendCommand.Run returned error: %v", suspendOutcome.err)
	}
	if !strings.Contains(suspendOutcome.stdout, "sandbox "+sandboxID+" suspended") {
		t.Fatalf("expected suspend output for %q, got %q", sandboxID, suspendOutcome.stdout)
	}
	if got, want := adapter.suspendCalls, 1; got != want {
		t.Fatalf("unexpected suspend call count: got %d want %d", got, want)
	}
	requireSandboxStatus(t, client, sandboxID, cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED)

	listOutcome := runSandboxListWithCapture(SandboxListCommand{
		clientFlags: clientFlags{Host: host},
	}, runtimeContext{CWD: cwd})
	if listOutcome.cause != nil {
		t.Fatalf("capture failure: %v", listOutcome.cause)
	}
	if listOutcome.err != nil {
		t.Fatalf("SandboxListCommand.Run returned error: %v", listOutcome.err)
	}
	if !strings.Contains(listOutcome.stdout, "suspended") {
		t.Fatalf("expected list output to include suspended status, got %q", listOutcome.stdout)
	}

	resumeOutcome := runSandboxResumeWithCapture(SandboxResumeCommand{
		clientFlags: clientFlags{Host: host},
		SandboxID:   sandboxID,
	}, runtimeContext{CWD: cwd})
	if resumeOutcome.cause != nil {
		t.Fatalf("capture failure: %v", resumeOutcome.cause)
	}
	if resumeOutcome.err != nil {
		t.Fatalf("SandboxResumeCommand.Run returned error: %v", resumeOutcome.err)
	}
	if !strings.Contains(resumeOutcome.stdout, "sandbox "+sandboxID+" resumed") {
		t.Fatalf("expected resume output for %q, got %q", sandboxID, resumeOutcome.stdout)
	}
	if got, want := adapter.resumeCalls, 1; got != want {
		t.Fatalf("unexpected resume call count: got %d want %d", got, want)
	}
	requireSandboxStatus(t, client, sandboxID, cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY)
}

func TestSandboxStatusStringIncludesSuspendStates(t *testing.T) {
	tests := map[cleanroomv1.SandboxStatus]string{
		cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDING: "suspending",
		cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED:  "suspended",
		cleanroomv1.SandboxStatus_SANDBOX_STATUS_WAKING:     "waking",
	}
	for status, want := range tests {
		if got := sandboxStatusString(status); got != want {
			t.Fatalf("sandboxStatusString(%v) = %q, want %q", status, got, want)
		}
	}
}

func TestSandboxSuspendCapabilityKeyIsStable(t *testing.T) {
	if got, want := backend.CapabilitySandboxSuspend, "sandbox.suspend"; got != want {
		t.Fatalf("unexpected suspend capability key: got %q want %q", got, want)
	}
}
