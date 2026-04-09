package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
)

type delayedPersistentAdapter struct {
	provisionDelay time.Duration
	runResult      backend.ExecutionResult
}

func (a *delayedPersistentAdapter) Name() string { return "firecracker" }

func (a *delayedPersistentAdapter) ProvisionSandbox(ctx context.Context, _ backend.ProvisionRequest) error {
	select {
	case <-time.After(a.provisionDelay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *delayedPersistentAdapter) RunInSandbox(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
	result := a.runResult
	result.ExecutionID = req.ExecutionID
	if stream.OnStdout != nil && result.Stdout != "" {
		stream.OnStdout([]byte(result.Stdout))
	}
	if stream.OnStderr != nil && result.Stderr != "" {
		stream.OnStderr([]byte(result.Stderr))
	}
	return &result, nil
}

func (a *delayedPersistentAdapter) TerminateSandbox(context.Context, string) error {
	return nil
}

func forceProgressTTY(t *testing.T) {
	t.Helper()

	oldIsTerminal := isTerminalFunc
	oldDelay := sandboxProgressStartDelay
	oldTick := sandboxProgressTickInterval

	isTerminalFunc = func(int) bool { return true }
	sandboxProgressStartDelay = 5 * time.Millisecond
	sandboxProgressTickInterval = 5 * time.Millisecond

	t.Cleanup(func() {
		isTerminalFunc = oldIsTerminal
		sandboxProgressStartDelay = oldDelay
		sandboxProgressTickInterval = oldTick
	})
}

func TestSandboxCreateIntegrationShowsProgressWhileProvisioning(t *testing.T) {
	forceProgressTTY(t)

	host, _ := startIntegrationServer(t, &delayedPersistentAdapter{
		provisionDelay: 25 * time.Millisecond,
	})
	cwd := t.TempDir()

	outcome := runSandboxCreateWithCapture(SandboxCreateCommand{
		clientFlags: clientFlags{Host: host},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("SandboxCreateCommand.Run returned error: %v", outcome.err)
	}
	if strings.TrimSpace(outcome.stdout) == "" {
		t.Fatalf("expected sandbox id output, got %q", outcome.stdout)
	}
	if !strings.Contains(outcome.stderr, "Preparing sandbox") {
		t.Fatalf("expected progress message in stderr, got %q", outcome.stderr)
	}
	if !strings.Contains(outcome.stderr, "Sandbox ready in") {
		t.Fatalf("expected completion message in stderr, got %q", outcome.stderr)
	}
}

func TestExecIntegrationShowsProgressWhenCreatingSandbox(t *testing.T) {
	forceProgressTTY(t)

	host, _ := startIntegrationServer(t, &delayedPersistentAdapter{
		provisionDelay: 25 * time.Millisecond,
		runResult: backend.ExecutionResult{
			ExitCode: 0,
			Stdout:   "ok\n",
			Message:  "ok",
		},
	})
	cwd := t.TempDir()

	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Command:     []string{"echo", "ignored-by-adapter"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	if !strings.Contains(outcome.stdout, "ok\n") {
		t.Fatalf("expected command output, got %q", outcome.stdout)
	}
	if !strings.Contains(outcome.stderr, "Preparing sandbox") {
		t.Fatalf("expected progress message in stderr, got %q", outcome.stderr)
	}
	if !strings.Contains(outcome.stderr, "Sandbox ready in") {
		t.Fatalf("expected completion message in stderr, got %q", outcome.stderr)
	}
}
