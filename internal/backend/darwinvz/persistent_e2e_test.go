//go:build darwin

package darwinvz

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/hosttools"
	"github.com/buildkite/cleanroom/internal/policy"
)

const (
	darwinVZE2EEnvEnabled     = "CLEANROOM_DARWIN_VZ_E2E"
	darwinVZE2EEnvImageRef    = "CLEANROOM_DARWIN_VZ_E2E_IMAGE_REF"
	darwinVZE2EEnvKernelImage = "CLEANROOM_DARWIN_VZ_E2E_KERNEL_IMAGE"
	darwinVZE2EEnvRootFS      = "CLEANROOM_DARWIN_VZ_E2E_ROOTFS"
)

func TestPersistentSandboxE2E(t *testing.T) {
	if strings.TrimSpace(os.Getenv(darwinVZE2EEnvEnabled)) == "" {
		t.Skipf("set %s=1 to run real darwin-vz persistence e2e", darwinVZE2EEnvEnabled)
	}
	if testing.Short() {
		t.Skip("skipping darwin-vz e2e in short mode")
	}

	helperPath, err := resolveHelperBinaryPath()
	if err != nil {
		t.Fatalf("resolve helper binary: %v", err)
	}
	hasEntitlement, err := helperHasVirtualizationEntitlement(helperPath)
	if err != nil {
		t.Fatalf("verify helper entitlement: %v", err)
	}
	if !hasEntitlement {
		t.Fatalf("helper %q is missing com.apple.security.virtualization entitlement", helperPath)
	}
	if _, _, err := New().getGuestAgentBinary(); err != nil {
		t.Fatalf("resolve guest agent binary: %v", err)
	}

	rootFSOverride := strings.TrimSpace(os.Getenv(darwinVZE2EEnvRootFS))
	if rootFSOverride == "" {
		if _, err := hosttools.ResolveE2FSProgsBinary("mkfs.ext4"); err != nil {
			t.Fatalf("resolve mkfs.ext4: %v", err)
		}
		if _, err := hosttools.ResolveE2FSProgsBinary("debugfs"); err != nil {
			t.Fatalf("resolve debugfs: %v", err)
		}
	}

	imageRef := strings.TrimSpace(os.Getenv(darwinVZE2EEnvImageRef))
	if imageRef == "" {
		imageRef = "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	}

	cfg := backend.FirecrackerConfig{
		KernelImagePath: strings.TrimSpace(os.Getenv(darwinVZE2EEnvKernelImage)),
		RootFSPath:      rootFSOverride,
		VCPUs:           1,
		MemoryMiB:       1024,
		LaunchSeconds:   90,
	}
	compiled := &policy.CompiledPolicy{
		Version:        1,
		ImageRef:       imageRef,
		NetworkDefault: "deny",
	}
	sandboxID := fmt.Sprintf("cr-e2e-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	adapter := New()
	if err := adapter.ProvisionSandbox(ctx, backend.ProvisionRequest{
		SandboxID:         sandboxID,
		Policy:            compiled,
		FirecrackerConfig: cfg,
	}); err != nil {
		t.Fatalf("ProvisionSandbox returned error: %v", err)
	}

	adapter.sandboxMu.Lock()
	instance := adapter.sandboxes[sandboxID]
	adapter.sandboxMu.Unlock()
	if instance == nil {
		t.Fatal("expected provisioned sandbox instance")
	}
	sandboxRunDir := instance.RunDir

	terminated := false
	defer func() {
		if terminated {
			return
		}
		if err := adapter.TerminateSandbox(context.Background(), sandboxID); err != nil {
			t.Fatalf("deferred TerminateSandbox returned error: %v", err)
		}
	}()

	markerPath := fmt.Sprintf("/tmp/%s-marker.txt", sandboxID)
	markerValue := "darwin-vz-persisted-state"

	run1Dir := filepath.Join(t.TempDir(), "run-1")
	first, err := adapter.RunInSandbox(ctx, backend.RunRequest{
		SandboxID: sandboxID,
		RunID:     "run-1",
		Command: []string{
			"sh", "-lc", fmt.Sprintf("printf %s > %s", markerValue, markerPath),
		},
		Policy: compiled,
		FirecrackerConfig: backend.FirecrackerConfig{
			RunDir:        run1Dir,
			LaunchSeconds: cfg.LaunchSeconds,
		},
	}, backend.OutputStream{})
	if err != nil {
		t.Fatalf("first RunInSandbox returned error: %v", err)
	}
	if first.ExitCode != 0 {
		t.Fatalf("expected first exit code 0, got %d (stderr=%q)", first.ExitCode, first.Stderr)
	}
	if first.LaunchedVM {
		t.Fatalf("expected persistent execution to report LaunchedVM=false, got %#v", first)
	}

	run2Dir := filepath.Join(t.TempDir(), "run-2")
	second, err := adapter.RunInSandbox(ctx, backend.RunRequest{
		SandboxID: sandboxID,
		RunID:     "run-2",
		Command:   []string{"sh", "-lc", "cat " + markerPath},
		Policy:    compiled,
		FirecrackerConfig: backend.FirecrackerConfig{
			RunDir:        run2Dir,
			LaunchSeconds: cfg.LaunchSeconds,
		},
	}, backend.OutputStream{})
	if err != nil {
		t.Fatalf("second RunInSandbox returned error: %v", err)
	}
	if second.ExitCode != 0 {
		t.Fatalf("expected second exit code 0, got %d (stderr=%q)", second.ExitCode, second.Stderr)
	}
	if second.LaunchedVM {
		t.Fatalf("expected persistent execution to report LaunchedVM=false, got %#v", second)
	}
	if got, want := strings.TrimSpace(second.Stdout), markerValue; got != want {
		t.Fatalf("unexpected persisted file contents: got %q want %q", got, want)
	}
	if got, want := second.PlanPath, first.PlanPath; got != want {
		t.Fatalf("expected stable sandbox plan path across runs: got %q want %q", got, want)
	}

	if err := adapter.TerminateSandbox(ctx, sandboxID); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
	terminated = true

	if _, err := os.Stat(sandboxRunDir); !os.IsNotExist(err) {
		t.Fatalf("expected sandbox runtime dir to be removed, got err=%v", err)
	}
	if _, err := adapter.RunInSandbox(context.Background(), backend.RunRequest{
		SandboxID: sandboxID,
		RunID:     "run-after-terminate",
		Command:   []string{"true"},
		Policy:    compiled,
	}, backend.OutputStream{}); err == nil || !strings.Contains(err.Error(), "unknown sandbox") {
		t.Fatalf("expected run after terminate to fail with unknown sandbox, got %v", err)
	}
}
