//go:build darwin

package darwinvz

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
)

func TestCapabilitiesExposePersistentSandboxWithoutFileDownload(t *testing.T) {
	t.Parallel()

	caps := backend.CapabilitiesForAdapter(New())

	if !caps[backend.CapabilitySandboxPersistent] {
		t.Fatalf("expected %s=true", backend.CapabilitySandboxPersistent)
	}
	if caps[backend.CapabilitySandboxFileDownload] {
		t.Fatalf("expected %s=false", backend.CapabilitySandboxFileDownload)
	}
}

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

	compiled := &policy.CompiledPolicy{
		NetworkDefault: "deny",
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- adapter.ProvisionSandbox(context.Background(), backend.ProvisionRequest{
			SandboxID: "cr-test",
			Policy:    compiled,
		})
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first provision to start")
	}

	err := adapter.ProvisionSandbox(context.Background(), backend.ProvisionRequest{
		SandboxID: "cr-test",
		Policy:    compiled,
	})
	if err == nil {
		t.Fatal("expected second provision to fail")
	}

	close(block)
	if err := <-errCh; err != nil {
		t.Fatalf("first provision returned error: %v", err)
	}
}

func TestRunInSandboxUsesRequestLaunchSecondsOverride(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	defer close(block)
	adapter := &Adapter{}
	adapter.executeInSandboxFn = func(bootCtx context.Context, _ context.Context, instance *sandboxInstance, req backend.RunRequest, _ backend.OutputStream) (*backend.RunResult, error) {
		if instance == nil || instance.SandboxID != "cr-test" {
			t.Fatalf("unexpected sandbox instance: %#v", instance)
		}
		select {
		case <-block:
			return nil, errors.New("unexpected unblock")
		case <-bootCtx.Done():
			return nil, bootCtx.Err()
		}
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID:      "cr-test",
			CommandTimeout: 1,
		},
	}

	start := time.Now()
	_, err := adapter.RunInSandbox(context.Background(), backend.RunRequest{
		SandboxID: "cr-test",
		RunID:     "run-timeout",
		Command:   []string{"echo", "hello"},
		Policy: &policy.CompiledPolicy{
			NetworkDefault: "deny",
		},
		FirecrackerConfig: backend.FirecrackerConfig{
			LaunchSeconds: 3,
		},
	}, backend.OutputStream{})
	if err == nil {
		t.Fatal("expected run to fail on timeout")
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Fatalf("expected launch-seconds override to increase timeout, elapsed=%s err=%v", elapsed, err)
	}
}

func TestTerminateSandboxRemovesRunDir(t *testing.T) {
	t.Parallel()

	runDir := t.TempDir()
	artifactPath := runDir + "/artifact.txt"
	if err := os.WriteFile(artifactPath, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	adapter := &Adapter{
		sandboxes: map[string]*sandboxInstance{
			"cr-test": {
				SandboxID: "cr-test",
				RunDir:    runDir,
			},
		},
	}

	if err := adapter.TerminateSandbox(context.Background(), "cr-test"); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
	if _, err := os.Stat(runDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected run dir to be removed, got err=%v", err)
	}
}
