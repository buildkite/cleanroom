//go:build linux

package volumestore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	zfsTransferE2EEnv        = "CLEANROOM_ZFS_TRANSFER_E2E"
	zfsTransferE2EDatasetEnv = "CLEANROOM_ZFS_TRANSFER_E2E_DATASET"
	zfsTransferE2EHelperEnv  = "CLEANROOM_ZFS_TRANSFER_E2E_HELPER"
)

func TestZFSDriverImportIncrementalSnapshotWithRealZFS(t *testing.T) {
	if os.Getenv(zfsTransferE2EEnv) != "1" {
		t.Skipf("set %s=1 and %s=<pool/cleanroom> to run the real ZFS transfer test", zfsTransferE2EEnv, zfsTransferE2EDatasetEnv)
	}

	datasetRoot := strings.Trim(strings.TrimSpace(os.Getenv(zfsTransferE2EDatasetEnv)), "/")
	if datasetRoot == "" {
		datasetRoot = strings.Trim(strings.TrimSpace(os.Getenv("CLEANROOM_ZFS_DATASET")), "/")
	}
	if datasetRoot == "" {
		t.Skipf("set %s=<pool/cleanroom> to run the real ZFS transfer test", zfsTransferE2EDatasetEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	runner := zfsIntegrationRunner{
		helperPath: strings.TrimSpace(os.Getenv(zfsTransferE2EHelperEnv)),
	}
	driver, err := NewZFSDriver(ZFSDriverOptions{
		DatasetRoot: datasetRoot,
		Runner:      runner,
	})
	if err != nil {
		t.Fatalf("NewZFSDriver returned error: %v", err)
	}

	testID := sanitizeZFSDatasetComponent(fmt.Sprintf("zfs-transfer-e2e-%d", time.Now().UnixNano()))
	cleanup := zfsIntegrationCleanup(t)

	rootfsDir := filepath.Join(t.TempDir(), "cleanroom", "firecracker", "runtime-rootfs")
	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		t.Fatalf("create rootfs dir: %v", err)
	}
	basePath := filepath.Join(rootfsDir, testID+"-base.ext4")
	if err := writeZFSIntegrationBaseImage(basePath); err != nil {
		t.Fatalf("write base image: %v", err)
	}
	childPath := filepath.Join(rootfsDir, testID+"-child.ext4")
	if err := os.WriteFile(childPath, []byte("cleanroom zfs transfer child mutation\n"), 0o644); err != nil {
		t.Fatalf("write child image: %v", err)
	}

	base, err := driver.EnsureBaseVolume(ctx, EnsureBaseVolumeRequest{
		BaseID:       testID + "-base",
		SourcePath:   basePath,
		MinimumBytes: 8 << 20,
	})
	if err != nil {
		t.Fatalf("EnsureBaseVolume returned error: %v", err)
	}
	cleanup("base dataset", func(ctx context.Context) error {
		return driver.DestroyVolume(ctx, DestroyVolumeRequest{VolumeRef: datasetFromSnapshotRef(base.Ref)})
	})

	baseDesc, err := driver.DescribeSnapshot(ctx, DescribeSnapshotRequest{SnapshotRef: base.Ref})
	if err != nil {
		t.Fatalf("DescribeSnapshot(base) returned error: %v", err)
	}

	volume, err := driver.CreateWritableVolume(ctx, CreateWritableVolumeRequest{
		VolumeID: testID + "-source",
		BaseRef:  base.Ref,
	})
	if err != nil {
		t.Fatalf("CreateWritableVolume returned error: %v", err)
	}
	cleanup("source volume", func(ctx context.Context) error {
		return driver.DestroyVolume(ctx, DestroyVolumeRequest{VolumeRef: volume.Ref})
	})

	if err := runner.Run(ctx, "dd", "if="+childPath, "of="+volume.AttachmentPath, "bs=4M", "conv=fsync", "status=none"); err != nil {
		t.Fatalf("mutate writable volume: %v", err)
	}

	child, err := driver.SnapshotVolume(ctx, SnapshotVolumeRequest{
		SnapshotID: testID + "-child",
		VolumeRef:  volume.Ref,
	})
	if err != nil {
		t.Fatalf("SnapshotVolume returned error: %v", err)
	}
	cleanup("child snapshot", func(ctx context.Context) error {
		return driver.DestroySnapshot(ctx, DestroySnapshotRequest{SnapshotRef: child.StorageRef})
	})

	childDesc, err := driver.DescribeSnapshot(ctx, DescribeSnapshotRequest{
		SnapshotRef:        child.StorageRef,
		ParentSnapshotGUID: baseDesc.SnapshotGUID,
	})
	if err != nil {
		t.Fatalf("DescribeSnapshot(child) returned error: %v", err)
	}
	if childDesc.SnapshotGUID == baseDesc.SnapshotGUID {
		t.Fatalf("expected child snapshot guid to differ from base guid %q", baseDesc.SnapshotGUID)
	}

	plan, err := driver.PlanIncrementalSnapshotExport(ctx, IncrementalSnapshotExportRequest{
		FromSnapshotRef:  base.Ref,
		FromSnapshotGUID: baseDesc.SnapshotGUID,
		ToSnapshotRef:    child.StorageRef,
		ToSnapshotGUID:   childDesc.SnapshotGUID,
	})
	if err != nil {
		t.Fatalf("PlanIncrementalSnapshotExport returned error: %v", err)
	}

	var stream bytes.Buffer
	if err := driver.ExportIncrementalSnapshot(ctx, plan, &stream); err != nil {
		t.Fatalf("ExportIncrementalSnapshot returned error: %v", err)
	}
	if stream.Len() == 0 {
		t.Fatal("expected non-empty zfs incremental stream")
	}

	imported, err := driver.ImportIncrementalSnapshot(ctx, IncrementalSnapshotImportRequest{
		SnapshotID:           testID + "-imported",
		ParentSnapshotRef:    base.Ref,
		ParentSnapshotGUID:   baseDesc.SnapshotGUID,
		ExpectedSnapshotGUID: childDesc.SnapshotGUID,
	}, bytes.NewReader(stream.Bytes()))
	if err != nil {
		t.Fatalf("ImportIncrementalSnapshot returned error: %v", err)
	}
	cleanup("imported snapshot", func(ctx context.Context) error {
		return driver.DestroySnapshot(ctx, DestroySnapshotRequest{SnapshotRef: imported.StorageRef})
	})

	wantImportedRef := driver.snapshotRef(driver.importDataset(testID+"-imported"), zfsManagedSnapshotName)
	if imported.StorageRef != wantImportedRef {
		t.Fatalf("unexpected imported storage ref: got %q want %q", imported.StorageRef, wantImportedRef)
	}

	importedClone, err := driver.CloneSnapshotToVolume(ctx, CloneSnapshotToVolumeRequest{
		VolumeID:    testID + "-imported-clone",
		SnapshotRef: imported.StorageRef,
	})
	if err != nil {
		t.Fatalf("CloneSnapshotToVolume(imported) returned error: %v", err)
	}
	cleanup("imported clone", func(ctx context.Context) error {
		return driver.DestroyVolume(ctx, DestroyVolumeRequest{VolumeRef: importedClone.Ref})
	})
}

func writeZFSIntegrationBaseImage(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	err = file.Truncate(4 << 20)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func zfsIntegrationCleanup(t *testing.T) func(string, func(context.Context) error) {
	t.Helper()

	type cleanupFn struct {
		name string
		fn   func(context.Context) error
	}
	var cleanups []cleanupFn
	t.Cleanup(func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := cleanups[i].fn(ctx)
			cancel()
			if err != nil {
				t.Logf("cleanup %s: %v", cleanups[i].name, err)
			}
		}
	})

	return func(name string, fn func(context.Context) error) {
		cleanups = append(cleanups, cleanupFn{name: name, fn: fn})
	}
}

type zfsIntegrationRunner struct {
	helperPath string
}

func (r zfsIntegrationRunner) Run(ctx context.Context, command string, args ...string) error {
	return r.run(ctx, nil, io.Discard, command, args...)
}

func (r zfsIntegrationRunner) Output(ctx context.Context, command string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	if err := r.run(ctx, nil, &stdout, command, args...); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

func (r zfsIntegrationRunner) OutputTo(ctx context.Context, dst io.Writer, command string, args ...string) error {
	return r.run(ctx, nil, dst, command, args...)
}

func (r zfsIntegrationRunner) InputFrom(ctx context.Context, src io.Reader, command string, args ...string) error {
	return r.run(ctx, src, io.Discard, command, args...)
}

func (r zfsIntegrationRunner) run(ctx context.Context, stdin io.Reader, stdout io.Writer, command string, args ...string) error {
	argv := append([]string{command}, args...)
	execArgv := argv
	if r.helperPath != "" {
		execArgv = append([]string{"sudo", "-n", r.helperPath}, argv...)
	}

	cmd := exec.CommandContext(ctx, execArgv[0], execArgv[1:]...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, msg)
		}
		return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	return nil
}
