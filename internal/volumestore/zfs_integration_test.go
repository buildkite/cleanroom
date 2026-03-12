package volumestore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// realZFSRunner executes ZFS commands via sudo for integration testing.
type realZFSRunner struct{}

func (r realZFSRunner) Run(ctx context.Context, command string, args ...string) error {
	cmd := exec.CommandContext(ctx, "sudo", append([]string{"-n", command}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w (%s)", command, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (r realZFSRunner) Output(ctx context.Context, command string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sudo", append([]string{"-n", command}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w (%s)", command, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func skipUnlessZFS(t *testing.T) string {
	t.Helper()
	if os.Getenv("CLEANROOM_ZFS_TEST_DATASET") == "" {
		t.Skip("set CLEANROOM_ZFS_TEST_DATASET to a writable ZFS dataset to run ZFS integration tests")
	}
	dataset := os.Getenv("CLEANROOM_ZFS_TEST_DATASET")
	// Verify the dataset is accessible.
	out, err := exec.Command("sudo", "-n", "zfs", "list", "-H", "-o", "name", dataset).CombinedOutput()
	if err != nil {
		t.Skipf("ZFS dataset %q not accessible: %s", dataset, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(string(out)) != dataset {
		t.Skipf("ZFS dataset probe returned %q, expected %q", strings.TrimSpace(string(out)), dataset)
	}
	return dataset
}

func TestZFSIntegrationFullLifecycle(t *testing.T) {
	dataset := skipUnlessZFS(t)

	// Use a sub-dataset unique to this test run to avoid collisions.
	testDataset := dataset + "/itest-lifecycle"
	runner := realZFSRunner{}

	// Clean up the test sub-dataset at the end.
	t.Cleanup(func() {
		_ = runner.Run(context.Background(), "zfs", "destroy", "-r", testDataset)
	})

	driver, err := NewZFSDriver(ZFSDriverOptions{
		DatasetRoot: testDataset,
		Runner:      runner,
	})
	if err != nil {
		t.Fatalf("NewZFSDriver: %v", err)
	}

	// Create a small ext4-ish source file (just raw bytes for the test).
	sourceDir := t.TempDir()
	sourcePath := sourceDir + "/test.ext4"
	// Write 8 MiB so the zvol has room.
	data := make([]byte, 8*1024*1024)
	copy(data, []byte("cleanroom-zfs-integration-test-marker"))
	if err := os.WriteFile(sourcePath, data, 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	// 1. EnsureBaseVolume: creates zvol + seed snapshot
	base, err := driver.EnsureBaseVolume(context.Background(), EnsureBaseVolumeRequest{
		BaseID:     "test-base",
		SourcePath: sourcePath,
	})
	if err != nil {
		t.Fatalf("EnsureBaseVolume: %v", err)
	}
	t.Logf("base ref: %s", base.Ref)
	if !strings.Contains(base.Ref, "@seed") {
		t.Fatalf("expected base ref to contain @seed, got %q", base.Ref)
	}

	// Calling again should be idempotent.
	base2, err := driver.EnsureBaseVolume(context.Background(), EnsureBaseVolumeRequest{
		BaseID:     "test-base",
		SourcePath: sourcePath,
	})
	if err != nil {
		t.Fatalf("EnsureBaseVolume (idempotent): %v", err)
	}
	if base.Ref != base2.Ref {
		t.Fatalf("idempotent call changed ref: %q vs %q", base.Ref, base2.Ref)
	}

	// 2. CreateWritableVolume: clone from base
	writable, err := driver.CreateWritableVolume(context.Background(), CreateWritableVolumeRequest{
		VolumeID: "sandbox-1",
		BaseRef:  base.Ref,
	})
	if err != nil {
		t.Fatalf("CreateWritableVolume: %v", err)
	}
	t.Logf("writable ref: %s, attachment: %s", writable.Ref, writable.AttachmentPath)
	if !strings.Contains(writable.AttachmentPath, "/dev/zvol/") {
		t.Fatalf("expected attachment path under /dev/zvol, got %q", writable.AttachmentPath)
	}

	// The driver waits for the device, so it should be present.
	if _, err := os.Stat(writable.AttachmentPath); err != nil {
		t.Fatalf("zvol device %q not available after create: %v", writable.AttachmentPath, err)
	}

	// Verify the seeded data is readable through the zvol device via sudo.
	marker := "cleanroom-zfs-integration-test-marker"
	out, err := exec.Command("sudo", "-n", "head", "-c", fmt.Sprintf("%d", len(marker)), writable.AttachmentPath).Output()
	if err != nil {
		t.Fatalf("read zvol device via sudo: %v", err)
	}
	if string(out) != marker {
		t.Fatalf("unexpected zvol data: got %q", string(out))
	}

	// 3. SnapshotVolume: snapshot the writable volume
	snapshot, err := driver.SnapshotVolume(context.Background(), SnapshotVolumeRequest{
		SnapshotID: "golden",
		VolumeRef:  writable.Ref,
	})
	if err != nil {
		t.Fatalf("SnapshotVolume: %v", err)
	}
	t.Logf("snapshot ref: %s", snapshot.Ref)
	if !strings.Contains(snapshot.Ref, "@snap-golden") {
		t.Fatalf("expected snapshot ref to contain @snap-golden, got %q", snapshot.Ref)
	}

	// 4. CloneSnapshotToVolume: fork from the snapshot
	fork, err := driver.CloneSnapshotToVolume(context.Background(), CloneSnapshotToVolumeRequest{
		VolumeID:    "sandbox-2",
		SnapshotRef: snapshot.Ref,
	})
	if err != nil {
		t.Fatalf("CloneSnapshotToVolume: %v", err)
	}
	t.Logf("fork ref: %s, attachment: %s", fork.Ref, fork.AttachmentPath)

	// 5. Destroy the fork volume
	if err := driver.DestroyVolume(context.Background(), DestroyVolumeRequest{VolumeRef: fork.Ref}); err != nil {
		t.Fatalf("DestroyVolume (fork): %v", err)
	}

	// 6. Destroy the snapshot
	if err := driver.DestroySnapshot(context.Background(), DestroySnapshotRequest{SnapshotRef: snapshot.Ref}); err != nil {
		t.Fatalf("DestroySnapshot: %v", err)
	}

	// 7. Destroy the original writable volume
	if err := driver.DestroyVolume(context.Background(), DestroyVolumeRequest{VolumeRef: writable.Ref}); err != nil {
		t.Fatalf("DestroyVolume (writable): %v", err)
	}

	// Verify idempotent destroy doesn't error.
	if err := driver.DestroyVolume(context.Background(), DestroyVolumeRequest{VolumeRef: writable.Ref}); err != nil {
		t.Fatalf("DestroyVolume (idempotent): %v", err)
	}
}

func TestZFSIntegrationSnapshotLineage(t *testing.T) {
	dataset := skipUnlessZFS(t)

	testDataset := dataset + "/itest-lineage"
	runner := realZFSRunner{}
	t.Cleanup(func() {
		_ = runner.Run(context.Background(), "zfs", "destroy", "-r", testDataset)
	})

	driver, err := NewZFSDriver(ZFSDriverOptions{
		DatasetRoot: testDataset,
		Runner:      runner,
	})
	if err != nil {
		t.Fatalf("NewZFSDriver: %v", err)
	}

	sourceDir := t.TempDir()
	sourcePath := sourceDir + "/test.ext4"
	data := make([]byte, 8*1024*1024)
	copy(data, []byte("lineage-test"))
	if err := os.WriteFile(sourcePath, data, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	base, err := driver.EnsureBaseVolume(context.Background(), EnsureBaseVolumeRequest{
		BaseID:     "lineage-base",
		SourcePath: sourcePath,
	})
	if err != nil {
		t.Fatalf("EnsureBaseVolume: %v", err)
	}

	// Create sandbox-1, snapshot it, fork to sandbox-2, snapshot sandbox-2.
	sb1, err := driver.CreateWritableVolume(context.Background(), CreateWritableVolumeRequest{
		VolumeID: "sb-lineage-1",
		BaseRef:  base.Ref,
	})
	if err != nil {
		t.Fatalf("CreateWritableVolume sb1: %v", err)
	}

	snap1, err := driver.SnapshotVolume(context.Background(), SnapshotVolumeRequest{
		SnapshotID: "snap1",
		VolumeRef:  sb1.Ref,
	})
	if err != nil {
		t.Fatalf("SnapshotVolume snap1: %v", err)
	}

	sb2, err := driver.CloneSnapshotToVolume(context.Background(), CloneSnapshotToVolumeRequest{
		VolumeID:    "sb-lineage-2",
		SnapshotRef: snap1.Ref,
	})
	if err != nil {
		t.Fatalf("CloneSnapshotToVolume sb2: %v", err)
	}

	snap2, err := driver.SnapshotVolume(context.Background(), SnapshotVolumeRequest{
		SnapshotID: "snap2",
		VolumeRef:  sb2.Ref,
	})
	if err != nil {
		t.Fatalf("SnapshotVolume snap2 (from forked sandbox): %v", err)
	}
	t.Logf("lineage: base=%s -> sb1=%s -> snap1=%s -> sb2=%s -> snap2=%s", base.Ref, sb1.Ref, snap1.Ref, sb2.Ref, snap2.Ref)

	// Clean up in reverse dependency order.
	if err := driver.DestroySnapshot(context.Background(), DestroySnapshotRequest{SnapshotRef: snap2.Ref}); err != nil {
		t.Fatalf("DestroySnapshot snap2: %v", err)
	}
	if err := driver.DestroyVolume(context.Background(), DestroyVolumeRequest{VolumeRef: sb2.Ref}); err != nil {
		t.Fatalf("DestroyVolume sb2: %v", err)
	}
	if err := driver.DestroySnapshot(context.Background(), DestroySnapshotRequest{SnapshotRef: snap1.Ref}); err != nil {
		t.Fatalf("DestroySnapshot snap1: %v", err)
	}
	if err := driver.DestroyVolume(context.Background(), DestroyVolumeRequest{VolumeRef: sb1.Ref}); err != nil {
		t.Fatalf("DestroyVolume sb1: %v", err)
	}
}
