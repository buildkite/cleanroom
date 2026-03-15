package cli

import (
	"context"
	"strings"
	"testing"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

func runSandboxListWithCapture(cmd SandboxListCommand, ctx runtimeContext) execOutcome {
	return runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, nil, ctx)
}

func TestSandboxListIntegrationHidesStoppedByDefault(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	cwd := t.TempDir()

	client := mustNewControlClient(t, host)
	readySandboxID := mustCreateSandbox(t, client)
	stoppedSandboxID := mustCreateSandbox(t, client)
	if _, err := client.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: stoppedSandboxID}); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}

	defaultOutcome := runSandboxListWithCapture(SandboxListCommand{
		clientFlags: clientFlags{Host: host},
	}, runtimeContext{CWD: cwd})
	if defaultOutcome.cause != nil {
		t.Fatalf("capture failure: %v", defaultOutcome.cause)
	}
	if defaultOutcome.err != nil {
		t.Fatalf("SandboxListCommand.Run returned error: %v", defaultOutcome.err)
	}
	if !strings.Contains(defaultOutcome.stdout, readySandboxID) {
		t.Fatalf("expected default list output to include ready sandbox %q, got %q", readySandboxID, defaultOutcome.stdout)
	}
	if strings.Contains(defaultOutcome.stdout, stoppedSandboxID) {
		t.Fatalf("expected default list output to hide stopped sandbox %q, got %q", stoppedSandboxID, defaultOutcome.stdout)
	}

	allOutcome := runSandboxListWithCapture(SandboxListCommand{
		clientFlags: clientFlags{Host: host},
		All:         true,
	}, runtimeContext{CWD: cwd})
	if allOutcome.cause != nil {
		t.Fatalf("capture failure: %v", allOutcome.cause)
	}
	if allOutcome.err != nil {
		t.Fatalf("SandboxListCommand.Run with --all returned error: %v", allOutcome.err)
	}
	if !strings.Contains(allOutcome.stdout, readySandboxID) {
		t.Fatalf("expected --all output to include ready sandbox %q, got %q", readySandboxID, allOutcome.stdout)
	}
	if !strings.Contains(allOutcome.stdout, stoppedSandboxID) {
		t.Fatalf("expected --all output to include stopped sandbox %q, got %q", stoppedSandboxID, allOutcome.stdout)
	}
	if !strings.Contains(allOutcome.stdout, "stopped") {
		t.Fatalf("expected --all output to include stopped status, got %q", allOutcome.stdout)
	}
}
