package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestProvisionSandboxRejectsConcurrentProvisionForSameID(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	started := make(chan struct{})
	adapter := &Adapter{
		launchSandboxVMFn: func(_ context.Context, sandboxID string, _ *policy.CompiledPolicy, _ backend.FirecrackerConfig) (*sandboxInstance, error) {
			if sandboxID != "cr-test" {
				t.Fatalf("unexpected sandbox id %q", sandboxID)
			}
			select {
			case started <- struct{}{}:
			default:
			}
			<-block
			return &sandboxInstance{SandboxID: sandboxID}, nil
		},
	}

	compiled := &policy.CompiledPolicy{NetworkDefault: "deny", ImageRef: "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	errCh := make(chan error, 1)
	go func() {
		errCh <- adapter.ProvisionSandbox(context.Background(), backend.ProvisionRequest{SandboxID: "cr-test", Policy: compiled})
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first provision to start")
	}

	err := adapter.ProvisionSandbox(context.Background(), backend.ProvisionRequest{SandboxID: "cr-test", Policy: compiled})
	if err == nil {
		t.Fatal("expected second provision to fail")
	}

	close(block)
	if err := <-errCh; err != nil {
		t.Fatalf("first provision returned error: %v", err)
	}
}

func TestSandboxShutdownRemovesRunDir(t *testing.T) {
	t.Parallel()

	runDir := t.TempDir()
	artifactPath := filepath.Join(runDir, "artifact.txt")
	if err := os.WriteFile(artifactPath, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	instance := &sandboxInstance{RunDir: runDir}
	instance.shutdown()

	if _, err := os.Stat(runDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected run dir to be removed, got err=%v", err)
	}
}

func TestRunInSandboxUsesRequestLaunchSecondsOverride(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	defer close(block)
	adapter := &Adapter{}
	adapter.runGuestCommandFn = func(bootCtx context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, _ vsockexec.ExecRequest, _ backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
		select {
		case <-block:
			return vsockexec.ExecResponse{}, guestExecTiming{}, errors.New("unexpected unblock")
		case <-bootCtx.Done():
			return vsockexec.ExecResponse{}, guestExecTiming{}, bootCtx.Err()
		}
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID:      "cr-test",
			VsockPath:      "/tmp/fake.sock",
			GuestPort:      10700,
			CommandTimeout: 1,
		},
	}

	start := time.Now()
	_, err := adapter.RunInSandbox(context.Background(), backend.RunRequest{
		SandboxID:         "cr-test",
		RunID:             "run-timeout",
		Command:           []string{"echo", "hello"},
		FirecrackerConfig: backend.FirecrackerConfig{LaunchSeconds: 3},
	}, backend.OutputStream{})
	if err == nil {
		t.Fatal("expected run to fail on timeout")
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Fatalf("expected launch-seconds override to increase timeout, elapsed=%s err=%v", elapsed, err)
	}
}

func TestRunInSandboxWritesRunObservabilityForStatusCommand(t *testing.T) {
	t.Parallel()

	runDir := t.TempDir()
	adapter := &Adapter{}
	adapter.runGuestCommandFn = func(_ context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, req vsockexec.ExecRequest, stream backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
		if !bytes.Equal([]byte(req.Command[0]), []byte("echo")) {
			t.Fatalf("unexpected command: %v", req.Command)
		}
		if stream.OnStdout != nil {
			stream.OnStdout([]byte("hello\n"))
		}
		return vsockexec.ExecResponse{ExitCode: 0, Stdout: "hello\n"}, guestExecTiming{WaitForAgent: 5 * time.Millisecond, CommandRun: 8 * time.Millisecond}, nil
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID:   "cr-test",
			VsockPath:   "/tmp/fake.sock",
			GuestPort:   10700,
			ConfigPath:  "/tmp/fake-config.json",
			ImageRef:    "image-ref",
			ImageDigest: "image-digest",
		},
	}

	result, err := adapter.RunInSandbox(context.Background(), backend.RunRequest{
		SandboxID: "cr-test",
		RunID:     "run-123",
		Command:   []string{"echo", "hello"},
		FirecrackerConfig: backend.FirecrackerConfig{
			RunDir: runDir,
		},
	}, backend.OutputStream{})
	if err != nil {
		t.Fatalf("RunInSandbox returned error: %v", err)
	}
	if got, want := result.RunDir, runDir; got != want {
		t.Fatalf("unexpected run dir in result: got %q want %q", got, want)
	}

	obsPath := filepath.Join(runDir, runObservabilityFile)
	b, err := os.ReadFile(obsPath)
	if err != nil {
		t.Fatalf("read observability file: %v", err)
	}
	var obs map[string]any
	if err := json.Unmarshal(b, &obs); err != nil {
		t.Fatalf("parse observability json: %v", err)
	}
	if got, want := obs["run_id"], "run-123"; got != want {
		t.Fatalf("unexpected run_id: got %v want %v", got, want)
	}
}

func TestRunInSandboxWritesRunObservabilityOnError(t *testing.T) {
	t.Parallel()

	runDir := t.TempDir()
	adapter := &Adapter{}
	adapter.runGuestCommandFn = func(_ context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, _ vsockexec.ExecRequest, _ backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
		return vsockexec.ExecResponse{}, guestExecTiming{}, errors.New("guest command failed")
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID:   "cr-test",
			VsockPath:   "/tmp/fake.sock",
			GuestPort:   10700,
			ConfigPath:  "/tmp/fake-config.json",
			ImageRef:    "image-ref",
			ImageDigest: "image-digest",
		},
	}

	_, err := adapter.RunInSandbox(context.Background(), backend.RunRequest{
		SandboxID: "cr-test",
		RunID:     "run-err",
		Command:   []string{"echo", "hello"},
		FirecrackerConfig: backend.FirecrackerConfig{
			RunDir: runDir,
		},
	}, backend.OutputStream{})
	if err == nil {
		t.Fatal("expected RunInSandbox to fail")
	}

	obsPath := filepath.Join(runDir, runObservabilityFile)
	b, readErr := os.ReadFile(obsPath)
	if readErr != nil {
		t.Fatalf("read observability file: %v", readErr)
	}
	var obs map[string]any
	if err := json.Unmarshal(b, &obs); err != nil {
		t.Fatalf("parse observability json: %v", err)
	}
	if got, want := obs["run_id"], "run-err"; got != want {
		t.Fatalf("unexpected run_id: got %v want %v", got, want)
	}
	if got := obs["guest_error"]; got == nil || got == "" {
		t.Fatalf("expected guest_error to be recorded, got %v", got)
	}
}

func TestSandboxRuntimeBaseDirUsesSeparateSandboxRoot(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	baseDir, err := sandboxRuntimeBaseDir()
	if err != nil {
		t.Fatalf("sandboxRuntimeBaseDir returned error: %v", err)
	}
	want := filepath.Join(stateHome, "cleanroom", "sandboxes")
	if got := baseDir; got != want {
		t.Fatalf("unexpected sandbox runtime base dir: got %q want %q", got, want)
	}
}

func TestDownloadSandboxFileReturnsBytes(t *testing.T) {
	t.Parallel()

	adapter := &Adapter{}
	adapter.runGuestCommandFn = func(_ context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, req vsockexec.ExecRequest, stream backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
		if got, want := req.Command, []string{"head", "-c", "33", "--", "/home/sprite/artifacts/haiku.txt"}; len(got) != len(want) || strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("unexpected command: got %v want %v", got, want)
		}
		if stream.OnStdout != nil {
			stream.OnStdout([]byte("hello"))
		}
		return vsockexec.ExecResponse{ExitCode: 0, Stdout: "hello"}, guestExecTiming{}, nil
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID: "cr-test",
			VsockPath: "/tmp/fake.sock",
			GuestPort: 10700,
			exitedCh:  make(chan struct{}),
		},
	}

	data, err := adapter.DownloadSandboxFile(context.Background(), "cr-test", "/home/sprite/artifacts/haiku.txt", 32)
	if err != nil {
		t.Fatalf("DownloadSandboxFile returned error: %v", err)
	}
	if got, want := string(data), "hello"; got != want {
		t.Fatalf("unexpected data: got %q want %q", got, want)
	}
}

func TestDownloadSandboxFileFallsBackToExecResponseStdout(t *testing.T) {
	t.Parallel()

	adapter := &Adapter{}
	adapter.runGuestCommandFn = func(_ context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, _ vsockexec.ExecRequest, _ backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
		return vsockexec.ExecResponse{ExitCode: 0, Stdout: "legacy-output"}, guestExecTiming{}, nil
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID: "cr-test",
			VsockPath: "/tmp/fake.sock",
			GuestPort: 10700,
			exitedCh:  make(chan struct{}),
		},
	}

	data, err := adapter.DownloadSandboxFile(context.Background(), "cr-test", "/home/sprite/artifacts/haiku.txt", 32)
	if err != nil {
		t.Fatalf("DownloadSandboxFile returned error: %v", err)
	}
	if got, want := string(data), "legacy-output"; got != want {
		t.Fatalf("unexpected data: got %q want %q", got, want)
	}
}

func TestDownloadSandboxFileEnforcesMaxBytes(t *testing.T) {
	t.Parallel()

	adapter := &Adapter{}
	adapter.runGuestCommandFn = func(_ context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, _ vsockexec.ExecRequest, stream backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
		if stream.OnStdout != nil {
			stream.OnStdout([]byte("0123456789"))
		}
		return vsockexec.ExecResponse{ExitCode: 0, Stdout: "0123456789"}, guestExecTiming{}, nil
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID: "cr-test",
			VsockPath: "/tmp/fake.sock",
			GuestPort: 10700,
			exitedCh:  make(chan struct{}),
		},
	}

	_, err := adapter.DownloadSandboxFile(context.Background(), "cr-test", "/home/sprite/artifacts/haiku.txt", 5)
	if err == nil {
		t.Fatal("expected max_bytes error")
	}
	if !strings.Contains(err.Error(), "exceeds max_bytes") {
		t.Fatalf("unexpected error: %v", err)
	}
}
