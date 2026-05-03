//go:build darwin

package darwinvz

import (
	"bytes"
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

func defaultDarwinVZE2EImageRef() string {
	return "docker.io/library/alpine@sha256:a4f4213abb84c497377b8544c81b3564f313746700372ec4fe84653e4fb03805"
}

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
		imageRef = defaultDarwinVZE2EImageRef()
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
	first, err := adapter.RunInSandbox(ctx, backend.ExecutionRequest{
		SandboxID:   sandboxID,
		ExecutionID: "run-1",
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
		t.Fatalf("expected first exit code 0, got %d", first.ExitCode)
	}
	if first.LaunchedVM {
		t.Fatalf("expected persistent execution to report LaunchedVM=false, got %#v", first)
	}

	run2Dir := filepath.Join(t.TempDir(), "run-2")
	var secondStdout bytes.Buffer
	second, err := adapter.RunInSandbox(ctx, backend.ExecutionRequest{
		SandboxID:   sandboxID,
		ExecutionID: "run-2",
		Command:     []string{"sh", "-lc", "cat " + markerPath},
		Policy:      compiled,
		FirecrackerConfig: backend.FirecrackerConfig{
			RunDir:        run2Dir,
			LaunchSeconds: cfg.LaunchSeconds,
		},
	}, backend.OutputStream{OnStdout: func(chunk []byte) {
		_, _ = secondStdout.Write(chunk)
	}})
	if err != nil {
		t.Fatalf("second RunInSandbox returned error: %v", err)
	}
	if second.ExitCode != 0 {
		t.Fatalf("expected second exit code 0, got %d", second.ExitCode)
	}
	if second.LaunchedVM {
		t.Fatalf("expected persistent execution to report LaunchedVM=false, got %#v", second)
	}
	if got, want := strings.TrimSpace(secondStdout.String()), markerValue; got != want {
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
	if _, err := adapter.RunInSandbox(context.Background(), backend.ExecutionRequest{
		SandboxID:   sandboxID,
		ExecutionID: "run-after-terminate",
		Command:     []string{"true"},
		Policy:      compiled,
	}, backend.OutputStream{}); err == nil || !strings.Contains(err.Error(), "unknown sandbox") {
		t.Fatalf("expected run after terminate to fail with unknown sandbox, got %v", err)
	}
}

func TestPersistentSandboxCacheOutputVolumeE2E(t *testing.T) {
	if strings.TrimSpace(os.Getenv(darwinVZE2EEnvEnabled)) == "" {
		t.Skipf("set %s=1 to run real darwin-vz cache output volume e2e", darwinVZE2EEnvEnabled)
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
		imageRef = defaultDarwinVZE2EImageRef()
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
	sandboxID := fmt.Sprintf("cr-e2e-cache-output-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	outputDir := "/var/lib/cleanroom-cache-output-e2e"
	adapter := New()
	if err := adapter.ProvisionSandbox(ctx, backend.ProvisionRequest{
		SandboxID: sandboxID,
		Policy:    compiled,
		CacheOutputVolumes: []backend.CacheOutputVolumeSpec{
			{
				Stage:     "dependency-volume",
				BlockName: "cache-output-e2e",
				CacheKey:  "dependency-volume:v1:cache-output-e2e",
				VolumeID:  "dependency-volume-cache-output-e2e",
				DirMappings: []backend.CacheOutputDirMapping{
					{GuestPath: outputDir, Subpath: "dirs/0"},
				},
			},
		},
		FirecrackerConfig: cfg,
	}); err != nil {
		t.Fatalf("ProvisionSandbox returned error: %v", err)
	}
	defer func() {
		if err := adapter.TerminateSandbox(context.Background(), sandboxID); err != nil {
			t.Fatalf("deferred TerminateSandbox returned error: %v", err)
		}
	}()

	writeValue := "darwin-vz-cache-output-volume"
	runPersistentCommand(ctx, t, adapter, sandboxID, compiled, cfg, "cache-output-write", nil, "sh", "-lc", fmt.Sprintf("printf '%s' > %s/value", writeValue, outputDir))
	var stdout bytes.Buffer
	runPersistentCommand(ctx, t, adapter, sandboxID, compiled, cfg, "cache-output-read", &stdout, "sh", "-lc", "cat "+outputDir+"/value")
	if got, want := stdout.String(), writeValue; got != want {
		t.Fatalf("unexpected cache output volume contents: got %q want %q", got, want)
	}
}

func runPersistentCommand(ctx context.Context, t *testing.T, adapter *Adapter, sandboxID string, compiled *policy.CompiledPolicy, cfg backend.FirecrackerConfig, executionID string, stdout *bytes.Buffer, command ...string) {
	t.Helper()

	stream := backend.OutputStream{}
	if stdout != nil {
		stream.OnStdout = func(chunk []byte) {
			_, _ = stdout.Write(chunk)
		}
	}
	result, err := adapter.RunInSandbox(ctx, backend.ExecutionRequest{
		SandboxID:         sandboxID,
		ExecutionID:       executionID,
		Command:           command,
		Policy:            compiled,
		FirecrackerConfig: cfg,
	}, stream)
	if err != nil {
		t.Fatalf("%s RunInSandbox returned error: %v", executionID, err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("%s exit code = %d, want 0", executionID, result.ExitCode)
	}
}

func TestPersistentSandboxE2EExecStreamingDoesNotHang(t *testing.T) {
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
		imageRef = defaultDarwinVZE2EImageRef()
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
	sandboxID := fmt.Sprintf("cr-e2e-streaming-%d", time.Now().UnixNano())
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

	defer func() {
		if err := adapter.TerminateSandbox(context.Background(), sandboxID); err != nil {
			t.Fatalf("deferred TerminateSandbox returned error: %v", err)
		}
	}()

	// A quick warm-up run that exercises the readiness/proxy path before we
	// assert repeated non-interactive executions complete promptly.
	warmupCtx, warmupCancel := context.WithTimeout(ctx, 60*time.Second)
	warmup, err := adapter.RunInSandbox(warmupCtx, backend.ExecutionRequest{
		SandboxID:   sandboxID,
		ExecutionID: "run-warmup",
		Command:     []string{"sh", "-lc", "echo warmup"},
		Policy:      compiled,
		FirecrackerConfig: backend.FirecrackerConfig{
			LaunchSeconds: cfg.LaunchSeconds,
		},
	}, backend.OutputStream{})
	warmupCancel()
	if err != nil {
		t.Fatalf("warm-up RunInSandbox returned error: %v", err)
	}
	if warmup.ExitCode != 0 {
		t.Fatalf("expected warm-up exit code 0, got %d", warmup.ExitCode)
	}

	for i := 0; i < 8; i++ {
		runID := fmt.Sprintf("run-stream-%d", i)
		want := fmt.Sprintf("stream-%d", i)
		runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
		var stdout bytes.Buffer
		res, err := adapter.RunInSandbox(runCtx, backend.ExecutionRequest{
			SandboxID:   sandboxID,
			ExecutionID: runID,
			Command:     []string{"sh", "-lc", "echo " + want},
			Policy:      compiled,
			FirecrackerConfig: backend.FirecrackerConfig{
				LaunchSeconds: cfg.LaunchSeconds,
			},
		}, backend.OutputStream{OnStdout: func(chunk []byte) {
			_, _ = stdout.Write(chunk)
		}})
		runCancel()
		if err != nil {
			t.Fatalf("RunInSandbox %q returned error: %v", runID, err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("expected %q exit code 0, got %d", runID, res.ExitCode)
		}
		if got := strings.TrimSpace(stdout.String()); got != want {
			t.Fatalf("unexpected stdout for %q: got %q want %q", runID, got, want)
		}
	}
}
