//go:build darwin

package volumestore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const benchmarkVolumeBytes = 256 * 1024 * 1024

func BenchmarkDarwinSnapshotDrivers(b *testing.B) {
	drivers := map[string]func(string) (Driver, error){
		"file": func(snapshotBaseDir string) (Driver, error) {
			return NewFileDriver(FileDriverOptions{
				SnapshotBaseDir: snapshotBaseDir,
				Namespace:       "darwin-vz",
			})
		},
		"apfs": func(snapshotBaseDir string) (Driver, error) {
			return NewAPFSDriver(APFSDriverOptions{
				SnapshotBaseDir: snapshotBaseDir,
				Namespace:       "darwin-vz",
			})
		},
	}

	for driverName, factory := range drivers {
		driverName := driverName
		factory := factory
		b.Run(driverName, func(b *testing.B) {
			workDir := b.TempDir()
			snapshotBaseDir := filepath.Join(workDir, "snapshots")
			driver, err := factory(snapshotBaseDir)
			if err != nil {
				b.Fatalf("create %s driver: %v", driverName, err)
			}

			sourcePath := filepath.Join(workDir, "source.ext4")
			writeBenchmarkVolume(b, sourcePath, benchmarkVolumeBytes)

			b.Run("snapshot", func(b *testing.B) {
				benchmarkSnapshotVolume(b, driver, sourcePath, benchmarkVolumeBytes)
			})
			b.Run("clone", func(b *testing.B) {
				benchmarkCloneSnapshotToVolume(b, driver, sourcePath, benchmarkVolumeBytes)
			})
		})
	}
}

func benchmarkSnapshotVolume(b *testing.B, driver Driver, volumePath string, size int64) {
	ctx := context.Background()
	b.ReportAllocs()
	b.SetBytes(size)

	for i := 0; i < b.N; i++ {
		snapshotID := fmt.Sprintf("snapshot-%d", i)
		b.StartTimer()
		snapshot, err := driver.SnapshotVolume(ctx, SnapshotVolumeRequest{
			SnapshotID: snapshotID,
			VolumeRef:  volumePath,
		})
		b.StopTimer()
		if err != nil {
			b.Fatalf("SnapshotVolume(%q): %v", snapshotID, err)
		}
		if err := driver.DestroySnapshot(ctx, DestroySnapshotRequest{SnapshotRef: snapshot.Ref}); err != nil {
			b.Fatalf("DestroySnapshot(%q): %v", snapshotID, err)
		}
	}
}

func benchmarkCloneSnapshotToVolume(b *testing.B, driver Driver, volumePath string, size int64) {
	ctx := context.Background()
	snapshot, err := driver.SnapshotVolume(ctx, SnapshotVolumeRequest{
		SnapshotID: "bench-snapshot",
		VolumeRef:  volumePath,
	})
	if err != nil {
		b.Fatalf("prepare clone benchmark snapshot: %v", err)
	}
	defer func() {
		if err := driver.DestroySnapshot(ctx, DestroySnapshotRequest{SnapshotRef: snapshot.Ref}); err != nil {
			b.Fatalf("cleanup clone benchmark snapshot: %v", err)
		}
	}()

	b.ReportAllocs()
	b.SetBytes(size)

	for i := 0; i < b.N; i++ {
		attachmentPath := filepath.Join(b.TempDir(), fmt.Sprintf("clone-%d.ext4", i))
		b.StartTimer()
		volume, err := driver.CloneSnapshotToVolume(ctx, CloneSnapshotToVolumeRequest{
			VolumeID:       fmt.Sprintf("volume-%d", i),
			SnapshotRef:    snapshot.Ref,
			AttachmentPath: attachmentPath,
		})
		b.StopTimer()
		if err != nil {
			b.Fatalf("CloneSnapshotToVolume(%q): %v", attachmentPath, err)
		}
		if err := driver.DestroyVolume(ctx, DestroyVolumeRequest{VolumeRef: volume.Ref}); err != nil {
			b.Fatalf("DestroyVolume(%q): %v", volume.Ref, err)
		}
	}
}

func writeBenchmarkVolume(b *testing.B, path string, size int64) {
	b.Helper()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		b.Fatalf("create benchmark volume %q: %v", path, err)
	}
	defer f.Close()

	chunk := make([]byte, 1024*1024)
	for i := range chunk {
		chunk[i] = byte(i)
	}

	for written := int64(0); written < size; written += int64(len(chunk)) {
		n := int64(len(chunk))
		if remaining := size - written; remaining < n {
			n = remaining
		}
		if _, err := f.Write(chunk[:int(n)]); err != nil {
			b.Fatalf("write benchmark volume %q: %v", path, err)
		}
	}
	if err := f.Sync(); err != nil {
		b.Fatalf("sync benchmark volume %q: %v", path, err)
	}
}
