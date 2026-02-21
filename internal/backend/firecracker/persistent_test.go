package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
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

	adapter := &Adapter{}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID:      "cr-test",
			VsockPath:      "/tmp/does-not-exist.sock",
			GuestPort:      10700,
			CommandTimeout: 1,
		},
	}

	start := time.Now()
	_, err := adapter.RunInSandbox(context.Background(), backend.RunRequest{
		SandboxID:         "cr-test",
		Command:           []string{"echo", "hello"},
		FirecrackerConfig: backend.FirecrackerConfig{LaunchSeconds: 3},
	}, backend.OutputStream{})
	if err == nil {
		t.Fatal("expected run to fail for missing vsock")
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Fatalf("expected launch-seconds override to increase timeout, elapsed=%s err=%v", elapsed, err)
	}
}
