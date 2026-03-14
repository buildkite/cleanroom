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

func TestSnapshotLifecycleE2E(t *testing.T) {
	if strings.TrimSpace(os.Getenv(darwinVZE2EEnvEnabled)) == "" {
		t.Skipf("set %s=1 to run real darwin-vz snapshot e2e", darwinVZE2EEnvEnabled)
	}
	if testing.Short() {
		t.Skip("skipping darwin-vz snapshot e2e in short mode")
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
		Snapshots: backend.SnapshotConfig{
			Enabled: true,
			Driver:  "file",
			BaseDir: filepath.Join(t.TempDir(), "snapshots"),
		},
	}
	compiled := &policy.CompiledPolicy{
		Version:        1,
		ImageRef:       imageRef,
		NetworkDefault: "deny",
	}

	runCommand := func(ctx context.Context, adapter *Adapter, sandboxID, runID string, command ...string) string {
		t.Helper()

		result, err := adapter.RunInSandbox(ctx, backend.RunRequest{
			SandboxID: sandboxID,
			RunID:     runID,
			Command:   command,
			Policy:    compiled,
			FirecrackerConfig: backend.FirecrackerConfig{
				LaunchSeconds: cfg.LaunchSeconds,
			},
		}, backend.OutputStream{})
		if err != nil {
			t.Fatalf("%s returned error: %v", runID, err)
		}
		if result.ExitCode != 0 {
			t.Fatalf("%s exit code=%d stdout=%q stderr=%q", runID, result.ExitCode, result.Stdout, result.Stderr)
		}
		return strings.TrimSpace(result.Stdout)
	}

	sandboxID := fmt.Sprintf("cr-snapshot-%d", time.Now().UnixNano())
	forkSandboxID := sandboxID + "-fork"
	snapshotID := fmt.Sprintf("snap-%d", time.Now().UnixNano())
	markerPath := fmt.Sprintf("/snapshot-%s-marker.txt", sandboxID)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	adapter := New()
	if err := adapter.ProvisionSandbox(ctx, backend.ProvisionRequest{
		SandboxID:         sandboxID,
		Policy:            compiled,
		FirecrackerConfig: cfg,
	}); err != nil {
		t.Fatalf("ProvisionSandbox returned error: %v", err)
	}
	terminatedSandbox := false
	defer func() {
		if terminatedSandbox {
			return
		}
		if err := adapter.TerminateSandbox(context.Background(), sandboxID); err != nil {
			t.Fatalf("deferred TerminateSandbox returned error: %v", err)
		}
	}()
	terminatedFork := false
	defer func() {
		if terminatedFork {
			return
		}
		if err := adapter.TerminateSandbox(context.Background(), forkSandboxID); err != nil && !strings.Contains(err.Error(), "unknown sandbox") {
			t.Fatalf("deferred TerminateSandbox for fork returned error: %v", err)
		}
	}()

	beforeValue := "snapshot-before"
	afterValue := "snapshot-after"
	forkValue := "fork-updated"

	runCommand(ctx, adapter, sandboxID, "run-before", "sh", "-lc", fmt.Sprintf("printf '%%s' %s > %s", beforeValue, markerPath))

	snapshot, err := adapter.CreateSnapshot(ctx, backend.SnapshotRequest{
		SandboxID:         sandboxID,
		SnapshotID:        snapshotID,
		FirecrackerConfig: cfg,
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}
	if _, err := os.Stat(snapshot.StorageRef); err != nil {
		t.Fatalf("stat snapshot storage ref: %v", err)
	}

	runCommand(ctx, adapter, sandboxID, "run-after", "sh", "-lc", fmt.Sprintf("printf '%%s' %s > %s", afterValue, markerPath))
	if got, want := runCommand(ctx, adapter, sandboxID, "cat-after", "sh", "-lc", "cat "+markerPath), afterValue; got != want {
		t.Fatalf("unexpected post-snapshot marker: got %q want %q", got, want)
	}

	if err := adapter.RestoreSandbox(ctx, backend.RestoreRequest{
		SandboxID:         sandboxID,
		SnapshotID:        snapshotID,
		StorageRef:        snapshot.StorageRef,
		Policy:            compiled,
		FirecrackerConfig: cfg,
	}); err != nil {
		t.Fatalf("RestoreSandbox returned error: %v", err)
	}
	if got, want := runCommand(ctx, adapter, sandboxID, "cat-restored", "sh", "-lc", "cat "+markerPath), beforeValue; got != want {
		t.Fatalf("unexpected restored marker: got %q want %q", got, want)
	}

	if err := adapter.ProvisionSandboxFromSnapshot(ctx, backend.ProvisionFromSnapshotRequest{
		SandboxID:         forkSandboxID,
		SnapshotID:        snapshotID,
		StorageRef:        snapshot.StorageRef,
		Policy:            compiled,
		FirecrackerConfig: cfg,
	}); err != nil {
		t.Fatalf("ProvisionSandboxFromSnapshot returned error: %v", err)
	}
	if got, want := runCommand(ctx, adapter, forkSandboxID, "cat-fork", "sh", "-lc", "cat "+markerPath), beforeValue; got != want {
		t.Fatalf("unexpected fork marker: got %q want %q", got, want)
	}

	runCommand(ctx, adapter, forkSandboxID, "run-fork", "sh", "-lc", fmt.Sprintf("printf '%%s' %s > %s", forkValue, markerPath))
	if got, want := runCommand(ctx, adapter, forkSandboxID, "cat-fork-updated", "sh", "-lc", "cat "+markerPath), forkValue; got != want {
		t.Fatalf("unexpected updated fork marker: got %q want %q", got, want)
	}
	if got, want := runCommand(ctx, adapter, sandboxID, "cat-restored-again", "sh", "-lc", "cat "+markerPath), beforeValue; got != want {
		t.Fatalf("unexpected restored sandbox marker after fork mutation: got %q want %q", got, want)
	}

	if err := adapter.DeleteSnapshot(ctx, backend.DeleteSnapshotRequest{
		SnapshotID:        snapshotID,
		StorageRef:        snapshot.StorageRef,
		FirecrackerConfig: cfg,
	}); err != nil {
		t.Fatalf("DeleteSnapshot returned error: %v", err)
	}
	if _, err := os.Stat(snapshot.StorageRef); !os.IsNotExist(err) {
		t.Fatalf("expected snapshot to be removed, got err=%v", err)
	}

	if err := adapter.TerminateSandbox(ctx, forkSandboxID); err != nil {
		t.Fatalf("TerminateSandbox for fork returned error: %v", err)
	}
	terminatedFork = true
	if err := adapter.TerminateSandbox(ctx, sandboxID); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
	terminatedSandbox = true
}
