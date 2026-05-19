//go:build darwin || linux

package volumestore

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestFileDriverPreservesSparseVolumeCopies(t *testing.T) {
	const logicalSize = 64 << 20

	sourcePath := filepath.Join(t.TempDir(), "prepared.ext4")
	writeSparseVolumeForTest(t, sourcePath, logicalSize)

	driver, err := NewFileDriver(FileDriverOptions{
		SnapshotBaseDir: t.TempDir(),
		Namespace:       "firecracker",
	})
	if err != nil {
		t.Fatalf("NewFileDriver returned error: %v", err)
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
		t.Fatalf("CreateWritableVolume returned error: %v", err)
	}
	requireSparseVolumeForTest(t, volume.AttachmentPath, logicalSize)

	snapshot, err := driver.SnapshotVolume(context.Background(), SnapshotVolumeRequest{
		SnapshotID: "snap-1",
		VolumeRef:  volume.AttachmentPath,
	})
	if err != nil {
		t.Fatalf("SnapshotVolume returned error: %v", err)
	}
	if got := snapshot.StorageSizeBytes; got != logicalSize {
		t.Fatalf("unexpected snapshot storage size: got %d want %d", got, int64(logicalSize))
	}
	if got := snapshot.ExclusiveSizeBytes; got <= 0 || got > logicalSize/4 {
		t.Fatalf("unexpected snapshot exclusive size: got %d logical %d", got, int64(logicalSize))
	}
	requireSparseVolumeForTest(t, snapshot.Ref, logicalSize)

	clone, err := driver.CloneSnapshotToVolume(context.Background(), CloneSnapshotToVolumeRequest{
		VolumeID:       "sandbox-2",
		SnapshotRef:    snapshot.Ref,
		AttachmentPath: filepath.Join(t.TempDir(), "sandbox-2.ext4"),
	})
	if err != nil {
		t.Fatalf("CloneSnapshotToVolume returned error: %v", err)
	}
	requireSparseVolumeForTest(t, clone.AttachmentPath, logicalSize)
}

func writeSparseVolumeForTest(t *testing.T, path string, logicalSize int64) {
	t.Helper()

	source, err := os.Create(path)
	if err != nil {
		t.Fatalf("create sparse source: %v", err)
	}
	if _, err := source.WriteAt([]byte("root"), 0); err != nil {
		t.Fatalf("write source head: %v", err)
	}
	if _, err := source.WriteAt([]byte("tail"), logicalSize-4096); err != nil {
		t.Fatalf("write source tail: %v", err)
	}
	if err := source.Truncate(logicalSize); err != nil {
		t.Fatalf("truncate source: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("close source: %v", err)
	}
	if allocated := allocatedSizeForTest(t, path); allocated > logicalSize/4 {
		t.Skipf("test filesystem did not keep source sparse enough: allocated %d of %d bytes", allocated, logicalSize)
	}
}

func requireSparseVolumeForTest(t *testing.T, path string, logicalSize int64) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat sparse volume: %v", err)
	}
	if got := info.Size(); got != logicalSize {
		t.Fatalf("unexpected logical size: got %d want %d", got, int64(logicalSize))
	}
	if allocated := allocatedSizeForTest(t, path); allocated > logicalSize/4 {
		t.Fatalf("writable volume did not stay sparse: allocated %d of %d bytes", allocated, logicalSize)
	}
	requireBytesAt(t, path, 0, []byte("root"))
	requireBytesAt(t, path, logicalSize-4096, []byte("tail"))
}

func allocatedSizeForTest(t *testing.T, path string) int64 {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %q did not include syscall.Stat_t", path)
	}
	return int64(stat.Blocks) * 512
}

func requireBytesAt(t *testing.T, path string, offset int64, want []byte) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %q: %v", path, err)
	}
	defer f.Close()

	got := make([]byte, len(want))
	if _, err := f.ReadAt(got, offset); err != nil {
		t.Fatalf("read %q at %d: %v", path, offset, err)
	}
	if string(got) != string(want) {
		t.Fatalf("unexpected bytes at %d: got %q want %q", offset, got, want)
	}
}
