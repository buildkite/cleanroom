package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
)

type delayedPersistentAdapter struct {
	provisionDelay  time.Duration
	progressMessage string
	runResult       backend.ExecutionResult
	runStdout       string
	runStderr       string
}

func (a *delayedPersistentAdapter) Name() string { return "firecracker" }

func (a *delayedPersistentAdapter) ProvisionSandbox(ctx context.Context, req backend.ProvisionRequest) error {
	if req.Progress != nil && a.progressMessage != "" {
		req.Progress(a.progressMessage)
	}
	select {
	case <-time.After(a.provisionDelay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSandboxCreateIntegrationShowsImageProgressMessages(t *testing.T) {
	forceProgressTTY(t)

	host, _ := startIntegrationServer(t, &delayedPersistentAdapter{
		provisionDelay:  25 * time.Millisecond,
		progressMessage: "resolving sandbox image rootfs...",
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
	if !strings.Contains(outcome.stderr, "resolving sandbox image rootfs...") {
		t.Fatalf("expected image progress message in stderr, got %q", outcome.stderr)
	}
}

func (a *delayedPersistentAdapter) RunInSandbox(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
	result := a.runResult
	result.ExecutionID = req.ExecutionID
	if stream.OnStdout != nil && a.runStdout != "" {
		stream.OnStdout([]byte(a.runStdout))
	}
	if stream.OnStderr != nil && a.runStderr != "" {
		stream.OnStderr([]byte(a.runStderr))
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
		t.Fatalf("expected sandbox progress output in stderr, got %q", outcome.stderr)
	}
	if strings.Contains(outcome.stderr, "provisioning sandbox") {
		t.Fatalf("expected sandbox phase chatter to be hidden, got %q", outcome.stderr)
	}
	if strings.Contains(outcome.stderr, "component=client") {
		t.Fatalf("expected structured client logs to be hidden, got %q", outcome.stderr)
	}
	if strings.Contains(outcome.stderr, "Sandbox ready in") {
		t.Fatalf("did not expect success completion message in stderr, got %q", outcome.stderr)
	}
}

func TestExecIntegrationShowsProgressWhenCreatingSandbox(t *testing.T) {
	forceProgressTTY(t)

	host, _ := startIntegrationServer(t, &delayedPersistentAdapter{
		provisionDelay: 25 * time.Millisecond,
		runResult: backend.ExecutionResult{
			ExitCode: 0,
			Message:  "ok",
		},
		runStdout: "ok\n",
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
		t.Fatalf("expected sandbox progress output in stderr, got %q", outcome.stderr)
	}
	if strings.Contains(outcome.stderr, "provisioning sandbox") {
		t.Fatalf("expected sandbox phase chatter to be hidden, got %q", outcome.stderr)
	}
	if strings.Contains(outcome.stderr, "component=client") {
		t.Fatalf("expected structured client logs to be hidden, got %q", outcome.stderr)
	}
	if strings.Contains(outcome.stderr, "Sandbox ready in") {
		t.Fatalf("did not expect success completion message in stderr, got %q", outcome.stderr)
	}
}
