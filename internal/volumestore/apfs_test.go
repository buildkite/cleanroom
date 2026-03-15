//go:build darwin

package volumestore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAPFSDriverWritableVolumeLifecycle(t *testing.T) {
	driver, err := NewAPFSDriver(APFSDriverOptions{
		SnapshotBaseDir: t.TempDir(),
		Namespace:       "darwin-vz",
	})
	if err != nil {
		skipIfClonefileUnsupported(t, err)
		t.Fatalf("NewAPFSDriver returned error: %v", err)
	}

	sourcePath := filepath.Join(t.TempDir(), "prepared.ext4")
	if err := os.WriteFile(sourcePath, []byte("base-bytes"), 0o644); err != nil {
		t.Fatalf("write source volume: %v", err)
	}

	base, err := driver.EnsureBaseVolume(context.Background(), EnsureBaseVolumeRequest{
		BaseID:     "runtime-key",
		SourcePath: sourcePath,
	})
	if err != nil {
		t.Fatalf("EnsureBaseVolume returned error: %v", err)
	}

	volume, err := driver.CreateWritableVolume(context.Background(), CreateWritableVolumeRequest{
		VolumeID:       "sandbox-1",
		BaseRef:        base.Ref,
		AttachmentPath: filepath.Join(t.TempDir(), "sandbox-1.ext4"),
	})
	if err != nil {
		skipIfClonefileUnsupported(t, err)
		t.Fatalf("CreateWritableVolume returned error: %v", err)
	}
	if got, want := volume.AttachmentPath, volume.Ref; got != want {
		t.Fatalf("unexpected writable attachment path: got %q want %q", got, want)
	}
	data, err := os.ReadFile(volume.AttachmentPath)
	if err != nil {
		t.Fatalf("read writable volume: %v", err)
	}
	if got, want := string(data), "base-bytes"; got != want {
		t.Fatalf("unexpected writable volume contents: got %q want %q", got, want)
	}

	if err := driver.DestroyVolume(context.Background(), DestroyVolumeRequest{VolumeRef: volume.Ref}); err != nil {
		t.Fatalf("DestroyVolume returned error: %v", err)
	}
	if _, err := os.Stat(volume.Ref); !os.IsNotExist(err) {
		t.Fatalf("expected writable volume to be removed, got %v", err)
	}
}

func TestAPFSDriverSnapshotLifecycle(t *testing.T) {
	snapshotBaseDir := t.TempDir()
	driver, err := NewAPFSDriver(APFSDriverOptions{
		SnapshotBaseDir: snapshotBaseDir,
		Namespace:       "darwin-vz",
	})
	if err != nil {
		skipIfClonefileUnsupported(t, err)
		t.Fatalf("NewAPFSDriver returned error: %v", err)
	}

	volumePath := filepath.Join(t.TempDir(), "sandbox.ext4")
	if err := os.WriteFile(volumePath, []byte("snapshot-bytes"), 0o644); err != nil {
		t.Fatalf("write volume: %v", err)
	}

	snapshot, err := driver.SnapshotVolume(context.Background(), SnapshotVolumeRequest{
		SnapshotID: "snap-1",
		VolumeRef:  volumePath,
	})
	if err != nil {
		skipIfClonefileUnsupported(t, err)
		t.Fatalf("SnapshotVolume returned error: %v", err)
	}
	if got, want := snapshot.StorageRef, filepath.Join(snapshotBaseDir, "darwin-vz", "snap-1", "rootfs.ext4"); got != want {
		t.Fatalf("unexpected snapshot storage ref: got %q want %q", got, want)
	}

	clone, err := driver.CloneSnapshotToVolume(context.Background(), CloneSnapshotToVolumeRequest{
		VolumeID:       "sandbox-2",
		SnapshotRef:    snapshot.Ref,
		AttachmentPath: filepath.Join(t.TempDir(), "sandbox-2.ext4"),
	})
	if err != nil {
		skipIfClonefileUnsupported(t, err)
		t.Fatalf("CloneSnapshotToVolume returned error: %v", err)
	}
	data, err := os.ReadFile(clone.AttachmentPath)
	if err != nil {
		t.Fatalf("read cloned volume: %v", err)
	}
	if got, want := string(data), "snapshot-bytes"; got != want {
		t.Fatalf("unexpected cloned volume contents: got %q want %q", got, want)
	}

	if err := driver.DestroySnapshot(context.Background(), DestroySnapshotRequest{SnapshotRef: snapshot.Ref}); err != nil {
		t.Fatalf("DestroySnapshot returned error: %v", err)
	}
	if _, err := os.Stat(snapshot.Ref); !os.IsNotExist(err) {
		t.Fatalf("expected snapshot to be removed, got %v", err)
	}
}

func TestAPFSDriverCreateWritableVolumeOverwritesExistingDestination(t *testing.T) {
	driver, err := NewAPFSDriver(APFSDriverOptions{
		SnapshotBaseDir: t.TempDir(),
		Namespace:       "darwin-vz",
	})
	if err != nil {
		skipIfClonefileUnsupported(t, err)
		t.Fatalf("NewAPFSDriver returned error: %v", err)
	}

	sourcePath := filepath.Join(t.TempDir(), "prepared.ext4")
	if err := os.WriteFile(sourcePath, []byte("fresh-bytes"), 0o644); err != nil {
		t.Fatalf("write source volume: %v", err)
	}

	base, err := driver.EnsureBaseVolume(context.Background(), EnsureBaseVolumeRequest{
		BaseID:     "runtime-key",
		SourcePath: sourcePath,
	})
	if err != nil {
		t.Fatalf("EnsureBaseVolume returned error: %v", err)
	}

	attachmentPath := filepath.Join(t.TempDir(), "sandbox-1.ext4")
	if err := os.WriteFile(attachmentPath, []byte("stale-bytes"), 0o644); err != nil {
		t.Fatalf("write stale attachment: %v", err)
	}

	volume, err := driver.CreateWritableVolume(context.Background(), CreateWritableVolumeRequest{
		VolumeID:       "sandbox-1",
		BaseRef:        base.Ref,
		AttachmentPath: attachmentPath,
	})
	if err != nil {
		skipIfClonefileUnsupported(t, err)
		t.Fatalf("CreateWritableVolume returned error: %v", err)
	}
	if got, want := volume.AttachmentPath, attachmentPath; got != want {
		t.Fatalf("unexpected writable attachment path: got %q want %q", got, want)
	}
	data, err := os.ReadFile(attachmentPath)
	if err != nil {
		t.Fatalf("read writable volume: %v", err)
	}
	if got, want := string(data), "fresh-bytes"; got != want {
		t.Fatalf("unexpected overwritten writable volume contents: got %q want %q", got, want)
	}
}

func skipIfClonefileUnsupported(t *testing.T, err error) {
	t.Helper()

	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENOSYS) {
		t.Skipf("clonefile is not supported on this filesystem: %v", err)
	}
}
