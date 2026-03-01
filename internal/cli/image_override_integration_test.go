package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
)

const (
	testImageOverrideRef = "ghcr.io/buildkite/cleanroom-base/alpine@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testImageOverrideTag = "ghcr.io/buildkite/cleanroom-base/alpine:latest"
)

func stubImageOverrideResolver(t *testing.T, fn func(context.Context, string) (string, error)) func() {
	t.Helper()
	prev := resolveReferenceForImageOverride
	resolveReferenceForImageOverride = fn
	return func() {
		resolveReferenceForImageOverride = prev
	}
}

func TestSandboxCreateIntegrationOverridesImageRefForNewSandbox(t *testing.T) {
	restore := stubImageOverrideResolver(t, func(_ context.Context, source string) (string, error) {
		if got, want := source, testImageOverrideTag; got != want {
			t.Fatalf("unexpected source passed to resolver: got %q want %q", got, want)
		}
		return testImageOverrideRef, nil
	})
	defer restore()

	imageRefCh := make(chan string, 1)
	adapter := &integrationAdapter{
		runFn: func(_ context.Context, req backend.RunRequest) (*backend.RunResult, error) {
			if req.Policy == nil {
				return nil, errors.New("expected policy on run request")
			}
			imageRefCh <- req.Policy.ImageRef
			return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Message: "ok"}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()

	createOutcome := runSandboxCreateWithCapture(SandboxCreateCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Image:       testImageOverrideTag,
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})
	if createOutcome.cause != nil {
		t.Fatalf("capture failure: %v", createOutcome.cause)
	}
	if createOutcome.err != nil {
		t.Fatalf("SandboxCreateCommand.Run returned error: %v", createOutcome.err)
	}
	sandboxID := strings.TrimSpace(createOutcome.stdout)
	if sandboxID == "" {
		t.Fatalf("expected sandbox id output, got %q", createOutcome.stdout)
	}

	execOutcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		SandboxID:   sandboxID,
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: failingLoader{},
	})
	if execOutcome.cause != nil {
		t.Fatalf("capture failure: %v", execOutcome.cause)
	}
	if execOutcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", execOutcome.err)
	}

	gotImageRef := mustReceiveWithin(t, imageRefCh, 2*time.Second, "timed out waiting for run request policy")
	if got, want := gotImageRef, testImageOverrideRef; got != want {
		t.Fatalf("unexpected image ref: got %q want %q", got, want)
	}
}

func TestExecIntegrationOverridesImageRefForCreatedSandbox(t *testing.T) {
	restore := stubImageOverrideResolver(t, func(_ context.Context, source string) (string, error) {
		if got, want := source, testImageOverrideTag; got != want {
			t.Fatalf("unexpected source passed to resolver: got %q want %q", got, want)
		}
		return testImageOverrideRef, nil
	})
	defer restore()

	imageRefCh := make(chan string, 1)
	adapter := &integrationAdapter{
		runFn: func(_ context.Context, req backend.RunRequest) (*backend.RunResult, error) {
			if req.Policy == nil {
				return nil, errors.New("expected policy on run request")
			}
			imageRefCh <- req.Policy.ImageRef
			return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Message: "ok"}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Image:       testImageOverrideTag,
		Command:     []string{"echo", "ok"},
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

	gotImageRef := mustReceiveWithin(t, imageRefCh, 2*time.Second, "timed out waiting for run request policy")
	if got, want := gotImageRef, testImageOverrideRef; got != want {
		t.Fatalf("unexpected image ref: got %q want %q", got, want)
	}
}

func TestConsoleIntegrationOverridesImageRefForCreatedSandbox(t *testing.T) {
	restore := stubImageOverrideResolver(t, func(_ context.Context, source string) (string, error) {
		if got, want := source, testImageOverrideTag; got != want {
			t.Fatalf("unexpected source passed to resolver: got %q want %q", got, want)
		}
		return testImageOverrideRef, nil
	})
	defer restore()

	imageRefCh := make(chan string, 1)
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.RunRequest, _ backend.OutputStream) (*backend.RunResult, error) {
			if req.Policy == nil {
				return nil, errors.New("expected policy on run request")
			}
			imageRefCh <- req.Policy.ImageRef
			return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Message: "ok"}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()
	outcome := runConsoleWithCapture(ConsoleCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Image:       testImageOverrideTag,
		Command:     []string{"sh"},
	}, "", runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ConsoleCommand.Run returned error: %v", outcome.err)
	}

	gotImageRef := mustReceiveWithin(t, imageRefCh, 2*time.Second, "timed out waiting for run request policy")
	if got, want := gotImageRef, testImageOverrideRef; got != want {
		t.Fatalf("unexpected image ref: got %q want %q", got, want)
	}
}

func TestExecIntegrationRejectsImageOverrideWhenSandboxProvided(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		SandboxID:   sandboxID,
		Image:       testImageOverrideRef,
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    t.TempDir(),
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected ExecCommand.Run to fail when both --sandbox-id and --image are set")
	}
	if got, want := outcome.err.Error(), "--image cannot be used with --sandbox-id"; !strings.Contains(got, want) {
		t.Fatalf("expected error to contain %q, got %q", want, got)
	}
}

func TestConsoleIntegrationRejectsImageOverrideWhenSandboxProvided(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.RunRequest, _ backend.OutputStream) (*backend.RunResult, error) {
			return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Message: "ok"}, nil
		},
	})
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	outcome := runConsoleWithCapture(ConsoleCommand{
		clientFlags: clientFlags{Host: host},
		SandboxID:   sandboxID,
		Image:       testImageOverrideRef,
		Command:     []string{"sh"},
	}, "", runtimeContext{
		CWD:    t.TempDir(),
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected ConsoleCommand.Run to fail when both --sandbox-id and --image are set")
	}
	if got, want := outcome.err.Error(), "--image cannot be used with --sandbox-id"; !strings.Contains(got, want) {
		t.Fatalf("expected error to contain %q, got %q", want, got)
	}
}
