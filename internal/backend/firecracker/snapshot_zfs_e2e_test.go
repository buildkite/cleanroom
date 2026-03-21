//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
)

const (
	firecrackerZFSE2EEnvEnabled     = "CLEANROOM_FIRECRACKER_ZFS_E2E"
	firecrackerZFSE2EEnvImageRef    = "CLEANROOM_FIRECRACKER_ZFS_E2E_IMAGE_REF"
	firecrackerZFSE2EEnvKernelImage = "CLEANROOM_FIRECRACKER_ZFS_E2E_KERNEL_IMAGE"
	firecrackerZFSE2EEnvBinary      = "CLEANROOM_FIRECRACKER_ZFS_E2E_BINARY"
	firecrackerZFSE2EEnvHelper      = "CLEANROOM_FIRECRACKER_ZFS_E2E_HELPER"
	firecrackerZFSE2EEnvDataset     = "CLEANROOM_FIRECRACKER_ZFS_E2E_ZFS_DATASET"
)

func defaultFirecrackerZFSE2EImageRef() string {
	return "docker.io/library/alpine@sha256:a4f4213abb84c497377b8544c81b3564f313746700372ec4fe84653e4fb03805"
}

func defaultFirecrackerZFSE2EDataset() string {
	if dataset := strings.TrimSpace(os.Getenv("CLEANROOM_ZFS_DATASET")); dataset != "" {
		return dataset
	}
	return "cleanroom/data"
}

func TestSnapshotLifecycleZFSE2E(t *testing.T) {
	if strings.TrimSpace(os.Getenv(firecrackerZFSE2EEnvEnabled)) == "" {
		t.Skipf("set %s=1 to run real firecracker zfs snapshot e2e", firecrackerZFSE2EEnvEnabled)
	}
	if testing.Short() {
		t.Skip("skipping firecracker zfs e2e in short mode")
	}

	imageRef := strings.TrimSpace(os.Getenv(firecrackerZFSE2EEnvImageRef))
	if imageRef == "" {
		imageRef = defaultFirecrackerZFSE2EImageRef()
	}
	cfg := backend.FirecrackerConfig{
		BinaryPath:           strings.TrimSpace(os.Getenv(firecrackerZFSE2EEnvBinary)),
		KernelImagePath:      strings.TrimSpace(os.Getenv(firecrackerZFSE2EEnvKernelImage)),
		PrivilegedHelperPath: strings.TrimSpace(os.Getenv(firecrackerZFSE2EEnvHelper)),
		VCPUs:                1,
		MemoryMiB:            1024,
		LaunchSeconds:        120,
		Snapshots: backend.SnapshotConfig{
			Enabled:               true,
			Driver:                "zfs",
			ZFSDataset:            strings.TrimSpace(os.Getenv(firecrackerZFSE2EEnvDataset)),
			QuiesceTimeoutSeconds: 15,
		},
	}
	if cfg.Snapshots.ZFSDataset == "" {
		cfg.Snapshots.ZFSDataset = defaultFirecrackerZFSE2EDataset()
	}

	compiled := &policy.CompiledPolicy{
		Version:        1,
		ImageRef:       imageRef,
		NetworkDefault: "deny",
	}
	adapter := New()

	report, err := adapter.Doctor(context.Background(), backend.DoctorRequest{
		Policy:            compiled,
		FirecrackerConfig: cfg,
	})
	if err != nil {
		t.Fatalf("Doctor returned error: %v", err)
	}
	if failures := firecrackerDoctorFailures(report); len(failures) > 0 {
		t.Fatalf("firecracker doctor reported failures:\n%s", strings.Join(failures, "\n"))
	}
	if got := firecrackerDoctorCheckStatus(report, "snapshot_zfs_dataset_access"); got != "pass" {
		t.Fatalf("expected snapshot_zfs_dataset_access=pass, got %q", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	sandboxID := fmt.Sprintf("cr-zfs-e2e-%d", time.Now().UnixNano())
	fromSnapshotSandboxID := sandboxID + "-from-snapshot"
	snapshotID := fmt.Sprintf("snap-%d", time.Now().UnixNano())
	markerPath := fmt.Sprintf("/tmp/%s-marker.txt", sandboxID)

	snapshotStorageRef := ""
	sourceVolumeRef := ""
	forkVolumeRef := ""
	terminatedSource := false
	terminatedFork := false
	deletedSnapshot := false

	defer func() {
		if !terminatedFork {
			if err := adapter.TerminateSandbox(context.Background(), fromSnapshotSandboxID); err != nil {
				t.Errorf("deferred TerminateSandbox for snapshot-backed sandbox: %v", err)
			}
		}
		if !terminatedSource {
			if err := adapter.TerminateSandbox(context.Background(), sandboxID); err != nil {
				t.Errorf("deferred TerminateSandbox for source sandbox: %v", err)
			}
		}
		if snapshotStorageRef != "" && !deletedSnapshot {
			if err := adapter.DeleteSnapshot(context.Background(), backend.DeleteSnapshotRequest{
				SnapshotID:        snapshotID,
				StorageRef:        snapshotStorageRef,
				FirecrackerConfig: cfg,
			}); err != nil {
				t.Errorf("deferred DeleteSnapshot: %v", err)
			}
		}
	}()

	if err := adapter.ProvisionSandbox(ctx, backend.ProvisionRequest{
		SandboxID:         sandboxID,
		Policy:            compiled,
		FirecrackerConfig: cfg,
	}); err != nil {
		t.Fatalf("ProvisionSandbox returned error: %v", err)
	}
	sourceVolumeRef = firecrackerSandboxVolumeRef(t, adapter, sandboxID)

	runCommand := func(ctx context.Context, sandboxID, runID string, command ...string) string {
		t.Helper()

		result, err := adapter.RunInSandbox(ctx, backend.ExecutionRequest{
			SandboxID:   sandboxID,
			ExecutionID: runID,
			Command:     command,
			Policy:      compiled,
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

	beforeValue := "snapshot-before"
	afterValue := "snapshot-after"
	fromSnapshotValue := "snapshot-backed-updated"

	runCommand(ctx, sandboxID, "run-before", "sh", "-lc", fmt.Sprintf("printf '%%s' %s > %s", beforeValue, markerPath))

	snapshot, err := adapter.CreateSnapshot(ctx, backend.SnapshotRequest{
		SandboxID:         sandboxID,
		SnapshotID:        snapshotID,
		FirecrackerConfig: cfg,
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}
	snapshotStorageRef = snapshot.StorageRef
	if snapshotStorageRef == "" {
		t.Fatal("expected snapshot storage ref")
	}
	firecrackerRequireZFSRefState(t, ctx, cfg, snapshotStorageRef, true)

	runCommand(ctx, sandboxID, "run-after", "sh", "-lc", fmt.Sprintf("printf '%%s' %s > %s", afterValue, markerPath))
	if got, want := runCommand(ctx, sandboxID, "cat-after", "sh", "-lc", "cat "+markerPath), afterValue; got != want {
		t.Fatalf("unexpected post-snapshot marker: got %q want %q", got, want)
	}

	if err := adapter.TerminateSandbox(ctx, sandboxID); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
	terminatedSource = true
	firecrackerRequireZFSRefState(t, ctx, cfg, sourceVolumeRef, false)
	firecrackerRequireZFSRefState(t, ctx, cfg, snapshotStorageRef, true)

	restoreCfg := cfg
	restoreCfg.Snapshots.ZFSDataset = ""
	if err := adapter.ProvisionSandboxFromSnapshot(ctx, backend.ProvisionFromSnapshotRequest{
		SandboxID:         fromSnapshotSandboxID,
		SnapshotID:        snapshotID,
		StorageRef:        snapshotStorageRef,
		Policy:            compiled,
		FirecrackerConfig: restoreCfg,
	}); err != nil {
		t.Fatalf("ProvisionSandboxFromSnapshot returned error: %v", err)
	}
	forkVolumeRef = firecrackerSandboxVolumeRef(t, adapter, fromSnapshotSandboxID)
	if got, want := runCommand(ctx, fromSnapshotSandboxID, "cat-from-snapshot", "sh", "-lc", "cat "+markerPath), beforeValue; got != want {
		t.Fatalf("unexpected snapshot-backed marker: got %q want %q", got, want)
	}

	runCommand(ctx, fromSnapshotSandboxID, "run-from-snapshot", "sh", "-lc", fmt.Sprintf("printf '%%s' %s > %s", fromSnapshotValue, markerPath))
	if got, want := runCommand(ctx, fromSnapshotSandboxID, "cat-from-snapshot-updated", "sh", "-lc", "cat "+markerPath), fromSnapshotValue; got != want {
		t.Fatalf("unexpected updated snapshot-backed marker: got %q want %q", got, want)
	}
	firecrackerRequireZFSRefState(t, ctx, cfg, snapshotStorageRef, true)

	if err := adapter.TerminateSandbox(ctx, fromSnapshotSandboxID); err != nil {
		t.Fatalf("TerminateSandbox for snapshot-backed sandbox returned error: %v", err)
	}
	terminatedFork = true
	firecrackerRequireZFSRefState(t, ctx, cfg, forkVolumeRef, false)
	firecrackerRequireZFSRefState(t, ctx, cfg, snapshotStorageRef, true)

	deleteCfg := cfg
	deleteCfg.Snapshots.ZFSDataset = ""
	if err := adapter.DeleteSnapshot(ctx, backend.DeleteSnapshotRequest{
		SnapshotID:        snapshotID,
		StorageRef:        snapshotStorageRef,
		FirecrackerConfig: deleteCfg,
	}); err != nil {
		t.Fatalf("DeleteSnapshot returned error: %v", err)
	}
	deletedSnapshot = true
	firecrackerRequireZFSRefState(t, ctx, cfg, snapshotStorageRef, false)
}

func firecrackerSandboxVolumeRef(t *testing.T, adapter *Adapter, sandboxID string) string {
	t.Helper()

	adapter.sandboxMu.Lock()
	defer adapter.sandboxMu.Unlock()

	instance := adapter.sandboxes[sandboxID]
	if instance == nil {
		t.Fatalf("expected sandbox %q to be provisioned", sandboxID)
	}
	if volumeRef := strings.TrimSpace(instance.volumeRef); volumeRef != "" {
		return volumeRef
	}
	t.Fatalf("sandbox %q is missing managed volume ref", sandboxID)
	return ""
}

func firecrackerRequireZFSRefState(t *testing.T, ctx context.Context, cfg backend.FirecrackerConfig, ref string, wantExists bool) {
	t.Helper()

	exists, err := firecrackerZFSRefExists(ctx, cfg, ref)
	if err != nil {
		t.Fatalf("inspect zfs ref %q: %v", ref, err)
	}
	if exists != wantExists {
		t.Fatalf("unexpected zfs ref state for %q: got exists=%t want exists=%t", ref, exists, wantExists)
	}
}

func firecrackerZFSRefExists(ctx context.Context, cfg backend.FirecrackerConfig, ref string) (bool, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false, nil
	}

	out, err := runRootCommandOutput(ctx, cfg, "zfs", "list", "-H", "-o", "name", ref)
	if err != nil {
		if firecrackerZFSMissingError(err) {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(string(out)) == ref, nil
}

func firecrackerZFSMissingError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "dataset does not exist") ||
		strings.Contains(msg, "no such pool or dataset") ||
		strings.Contains(msg, "snapshot does not exist") ||
		strings.Contains(msg, "could not find any snapshots to destroy")
}

func firecrackerDoctorCheckStatus(report *backend.DoctorReport, name string) string {
	if report == nil {
		return ""
	}
	for _, check := range report.Checks {
		if check.Name == name {
			return check.Status
		}
	}
	return ""
}

func firecrackerDoctorFailures(report *backend.DoctorReport) []string {
	if report == nil {
		return nil
	}
	failures := make([]string, 0)
	for _, check := range report.Checks {
		if check.Status != "fail" {
			continue
		}
		failures = append(failures, fmt.Sprintf("%s: %s", check.Name, check.Message))
	}
	return failures
}
