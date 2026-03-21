//go:build darwin

package darwinvz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestCapabilitiesExposePersistentSandboxWithoutFileDownload(t *testing.T) {
	t.Parallel()

	caps := backend.CapabilitiesForAdapter(New())

	if !caps[backend.CapabilitySandboxPersistent] {
		t.Fatalf("expected %s=true", backend.CapabilitySandboxPersistent)
	}
	if !caps[backend.CapabilitySandboxSnapshot] {
		t.Fatalf("expected %s=true", backend.CapabilitySandboxSnapshot)
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
	adapter.executeInSandboxFn = func(bootCtx context.Context, _ context.Context, instance *sandboxInstance, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
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
	_, err := adapter.RunInSandbox(context.Background(), backend.ExecutionRequest{
		SandboxID:   "cr-test",
		ExecutionID: "run-timeout",
		Command:     []string{"echo", "hello"},
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

func TestRunInSandboxRejectsExitedSandbox(t *testing.T) {
	t.Parallel()

	instance := &sandboxInstance{SandboxID: "cr-test", exitedCh: make(chan struct{})}
	instance.setExited(errors.New("helper crashed"))
	close(instance.exitedCh)

	adapter := &Adapter{
		sandboxes: map[string]*sandboxInstance{
			"cr-test": instance,
		},
	}

	_, err := adapter.RunInSandbox(context.Background(), backend.ExecutionRequest{
		SandboxID:   "cr-test",
		ExecutionID: "run-dead",
		Command:     []string{"true"},
		Policy:      &policy.CompiledPolicy{NetworkDefault: "deny"},
	}, backend.OutputStream{})
	if err == nil {
		t.Fatal("expected dead sandbox run to fail")
	}
	if got := err.Error(); got == "" || !bytes.Contains([]byte(got), []byte("not running")) {
		t.Fatalf("expected not running error, got %v", err)
	}
}

func TestProbeGuestExecReadyWaitsForGuestResponse(t *testing.T) {
	t.Parallel()

	socketDir, err := os.MkdirTemp("", "cr-probe-")
	if err != nil {
		t.Fatalf("create probe temp dir: %v", err)
	}
	defer os.RemoveAll(socketDir)

	socketPath := filepath.Join(socketDir, "p.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer listener.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			t.Errorf("accept probe connection: %v", acceptErr)
			return
		}
		defer conn.Close()

		var req vsockexec.ExecRequest
		decodeErr := json.NewDecoder(conn).Decode(&req)
		if decodeErr != nil {
			t.Errorf("decode probe request: %v", decodeErr)
			return
		}
		if len(req.Command) != 0 {
			t.Errorf("expected empty probe command, got %v", req.Command)
			return
		}
		if encodeErr := vsockexec.EncodeResponse(conn, vsockexec.ExecResponse{ExitCode: 1, Error: "missing command"}); encodeErr != nil {
			t.Errorf("encode probe response: %v", encodeErr)
		}
	}()

	if err := probeGuestExecReady(context.Background(), nil, socketPath); err != nil {
		t.Fatalf("probeGuestExecReady returned error: %v", err)
	}
	<-done
}

func TestDefaultDarwinVZE2EImageRefUsesPublishedDigest(t *testing.T) {
	t.Parallel()

	const want = "docker.io/library/alpine@sha256:a4f4213abb84c497377b8544c81b3564f313746700372ec4fe84653e4fb03805"
	if got := defaultDarwinVZE2EImageRef(); got != want {
		t.Fatalf("unexpected default e2e image ref: got %q want %q", got, want)
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
