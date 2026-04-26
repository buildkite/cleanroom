package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
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
			close(started)
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

func TestSandboxShutdownInvokesCleanupVolume(t *testing.T) {
	t.Parallel()

	called := false
	instance := &sandboxInstance{
		RunDir: t.TempDir(),
		cleanupVolume: func() {
			called = true
		},
	}
	instance.shutdown()

	if !called {
		t.Fatal("expected cleanupVolume to be invoked")
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
	_, err := adapter.RunInSandbox(context.Background(), backend.ExecutionRequest{
		SandboxID:         "cr-test",
		ExecutionID:       "run-timeout",
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
		return vsockexec.ExecResponse{ExitCode: 0}, guestExecTiming{WaitForAgent: 5 * time.Millisecond, CommandRun: 8 * time.Millisecond}, nil
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

	result, err := adapter.RunInSandbox(context.Background(), backend.ExecutionRequest{
		SandboxID:   "cr-test",
		ExecutionID: "run-123",
		Command:     []string{"echo", "hello"},
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
	if got, want := obs["execution_id"], "run-123"; got != want {
		t.Fatalf("unexpected execution_id: got %v want %v", got, want)
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

	_, err := adapter.RunInSandbox(context.Background(), backend.ExecutionRequest{
		SandboxID:   "cr-test",
		ExecutionID: "run-err",
		Command:     []string{"echo", "hello"},
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
	if got, want := obs["execution_id"], "run-err"; got != want {
		t.Fatalf("unexpected execution_id: got %v want %v", got, want)
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
		if len(req.Command) != 6 {
			t.Fatalf("unexpected command: got %v", req.Command)
		}
		if got, want := req.Command[0], "sh"; got != want {
			t.Fatalf("unexpected command[0]: got %q want %q", got, want)
		}
		if got, want := req.Command[3], "cleanroom-read"; got != want {
			t.Fatalf("unexpected command[3]: got %q want %q", got, want)
		}
		if got, want := req.Command[4], "/home/sprite/artifacts/haiku.txt"; got != want {
			t.Fatalf("unexpected read path: got %q want %q", got, want)
		}
		if got, want := req.Command[5], "33"; got != want {
			t.Fatalf("unexpected read limit: got %q want %q", got, want)
		}
		if stream.OnStdout != nil {
			stream.OnStdout([]byte("hello"))
		}
		return vsockexec.ExecResponse{ExitCode: 0}, guestExecTiming{}, nil
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

func TestDownloadSandboxFileHandlesMaxInt64MaxBytes(t *testing.T) {
	t.Parallel()

	adapter := &Adapter{}
	adapter.runGuestCommandFn = func(_ context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, req vsockexec.ExecRequest, stream backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
		if len(req.Command) != 6 {
			t.Fatalf("unexpected command: got %v", req.Command)
		}
		if got, want := req.Command[0], "sh"; got != want {
			t.Fatalf("unexpected command[0]: got %q want %q", got, want)
		}
		if got, want := req.Command[3], "cleanroom-read"; got != want {
			t.Fatalf("unexpected command[3]: got %q want %q", got, want)
		}
		if got, want := req.Command[4], "/home/sprite/artifacts/haiku.txt"; got != want {
			t.Fatalf("unexpected read path: got %q want %q", got, want)
		}
		if got, want := req.Command[5], "9223372036854775807"; got != want {
			t.Fatalf("unexpected read limit: got %q want %q", got, want)
		}
		if stream.OnStdout != nil {
			stream.OnStdout([]byte("hello"))
		}
		return vsockexec.ExecResponse{ExitCode: 0}, guestExecTiming{}, nil
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID: "cr-test",
			VsockPath: "/tmp/fake.sock",
			GuestPort: 10700,
			exitedCh:  make(chan struct{}),
		},
	}

	data, err := adapter.DownloadSandboxFile(context.Background(), "cr-test", "/home/sprite/artifacts/haiku.txt", math.MaxInt64)
	if err != nil {
		t.Fatalf("DownloadSandboxFile returned error: %v", err)
	}
	if got, want := string(data), "hello"; got != want {
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
		return vsockexec.ExecResponse{ExitCode: 0}, guestExecTiming{}, nil
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

func TestDownloadSandboxFileReturnsStreamedStderrOnFailure(t *testing.T) {
	t.Parallel()

	adapter := &Adapter{}
	adapter.runGuestCommandFn = func(_ context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, _ vsockexec.ExecRequest, stream backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
		if stream.OnStderr != nil {
			stream.OnStderr([]byte("path not found: /missing\n"))
		}
		return vsockexec.ExecResponse{ExitCode: 1}, guestExecTiming{}, nil
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID: "cr-test",
			VsockPath: "/tmp/fake.sock",
			GuestPort: 10700,
			exitedCh:  make(chan struct{}),
		},
	}

	_, err := adapter.DownloadSandboxFile(context.Background(), "cr-test", "/missing", 32)
	if err == nil {
		t.Fatal("expected download error")
	}
	if !errors.Is(err, backend.ErrSandboxPathNotFound) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWriteSandboxFileWritesPayloadModeAndMtime(t *testing.T) {
	t.Parallel()

	var got bytes.Buffer
	closed := false
	mtime := time.Unix(1700000123, 987654321)
	adapter := &Adapter{}
	adapter.runGuestCommandFn = func(_ context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, req vsockexec.ExecRequest, stream backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
		if got, want := req.Command[0], "sh"; got != want {
			t.Fatalf("unexpected command[0]: got %q want %q", got, want)
		}
		if got, want := req.Command[3], "cleanroom-copy"; got != want {
			t.Fatalf("unexpected command[3]: got %q want %q", got, want)
		}
		if got, want := req.Command[4], "/home/sprite/artifacts/upload.txt"; got != want {
			t.Fatalf("unexpected upload path: got %q want %q", got, want)
		}
		if got, want := req.Command[5], "0600"; got != want {
			t.Fatalf("unexpected upload mode: got %q want %q", got, want)
		}
		if got, want := req.Command[6], "1700000123"; got != want {
			t.Fatalf("unexpected upload mtime: got %q want %q", got, want)
		}
		if stream.OnAttach != nil {
			stream.OnAttach(backend.AttachIO{
				WriteStdin: func(data []byte) error {
					_, err := got.Write(data)
					return err
				},
				CloseStdin: func() error {
					closed = true
					return nil
				},
			})
		}
		return vsockexec.ExecResponse{ExitCode: 0}, guestExecTiming{}, nil
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID: "cr-test",
			VsockPath: "/tmp/fake.sock",
			GuestPort: 10700,
			exitedCh:  make(chan struct{}),
		},
	}

	written, err := adapter.WriteSandboxFile(context.Background(), "cr-test", "/home/sprite/artifacts/upload.txt", bytes.NewReader([]byte("payload")), 0o600, mtime)
	if err != nil {
		t.Fatalf("WriteSandboxFile returned error: %v", err)
	}
	if written != int64(len("payload")) {
		t.Fatalf("unexpected written byte count: got %d want %d", written, len("payload"))
	}
	if got.String() != "payload" {
		t.Fatalf("unexpected uploaded payload: got %q", got.String())
	}
	if !closed {
		t.Fatal("expected upload stdin to be closed")
	}
}

func TestWriteSandboxFileReturnsGuestErrorWhenUploadFailsBeforeReadingPayload(t *testing.T) {
	t.Parallel()

	writeStarted := make(chan struct{}, 1)
	unblockWrite := make(chan struct{})
	defer close(unblockWrite)
	adapter := &Adapter{}
	adapter.runGuestCommandFn = func(_ context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, _ vsockexec.ExecRequest, stream backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
		if stream.OnAttach != nil {
			stream.OnAttach(backend.AttachIO{
				WriteStdin: func([]byte) error {
					select {
					case writeStarted <- struct{}{}:
					default:
					}
					<-unblockWrite
					return errors.New("stdin closed")
				},
				CloseStdin: func() error {
					return nil
				},
			})
		}
		select {
		case <-writeStarted:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for upload copy to start")
		}
		if stream.OnStderr != nil {
			stream.OnStderr([]byte("destination is a directory: /tmp/upload\n"))
		}
		return vsockexec.ExecResponse{ExitCode: 1}, guestExecTiming{}, nil
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID: "cr-test",
			VsockPath: "/tmp/fake.sock",
			GuestPort: 10700,
			exitedCh:  make(chan struct{}),
		},
	}

	done := make(chan error, 1)
	go func() {
		_, err := adapter.WriteSandboxFile(context.Background(), "cr-test", "/tmp/upload", bytes.NewReader([]byte("payload")), 0o644, time.Time{})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected upload error")
		}
		if !strings.Contains(err.Error(), "destination is a directory") {
			t.Fatalf("unexpected upload error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WriteSandboxFile blocked behind upload stdin copy")
	}
}

func TestUploadSandboxFileRequiresStdinAttach(t *testing.T) {
	t.Parallel()

	adapter := &Adapter{}
	adapter.runGuestCommandFn = func(_ context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, _ vsockexec.ExecRequest, _ backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
		return vsockexec.ExecResponse{ExitCode: 0}, guestExecTiming{}, nil
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID: "cr-test",
			VsockPath: "/tmp/fake.sock",
			GuestPort: 10700,
			exitedCh:  make(chan struct{}),
		},
	}

	err := adapter.UploadSandboxFile(context.Background(), "cr-test", "/tmp/upload.txt", []byte("payload"), 0o644)
	if err == nil {
		t.Fatal("expected stdin attach error")
	}
	if !strings.Contains(err.Error(), "stdin attach") {
		t.Fatalf("unexpected error: %v", err)
	}
}
