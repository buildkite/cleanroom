//go:build darwin

package darwinvz

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
)

func TestWarmupUsesConfiguredRootFS(t *testing.T) {
	t.Parallel()

	kernelPath := writeWarmupTestFile(t, "kernel")
	rootFSPath := writeWarmupTestFile(t, "rootfs.ext4")
	snapshotBaseDir := t.TempDir()

	adapter := New()
	adapter.newImageManager = func() (imageEnsurer, error) {
		t.Fatal("configured rootfs warmup should not initialise image manager")
		return nil, nil
	}

	result, err := adapter.Warmup(context.Background(), backend.WarmupRequest{
		FirecrackerConfig: backend.FirecrackerConfig{
			KernelImagePath: kernelPath,
			RootFSPath:      rootFSPath,
			Snapshots: backend.SnapshotConfig{
				Driver:  "file",
				BaseDir: snapshotBaseDir,
			},
		},
	})
	if err != nil {
		t.Fatalf("Warmup returned error: %v", err)
	}

	if got, want := result.Backend, "darwin-vz"; got != want {
		t.Fatalf("unexpected backend: got %q want %q", got, want)
	}
	if got, want := result.KernelPath, kernelPath; got != want {
		t.Fatalf("unexpected kernel path: got %q want %q", got, want)
	}
	if got, want := result.KernelStatus, backend.WarmupStatusConfigured; got != want {
		t.Fatalf("unexpected kernel status: got %q want %q", got, want)
	}
	if got, want := result.RootFSPath, rootFSPath; got != want {
		t.Fatalf("unexpected rootfs path: got %q want %q", got, want)
	}
	if got, want := result.RootFSStatus, backend.WarmupStatusConfigured; got != want {
		t.Fatalf("unexpected rootfs status: got %q want %q", got, want)
	}
	if got, want := result.BaseRootFSRef, rootFSPath; got != want {
		t.Fatalf("unexpected base rootfs ref: got %q want %q", got, want)
	}
	if result.ImageRef != "" || result.ImageDigest != "" {
		t.Fatalf("configured rootfs warmup should not report derived image metadata, got ref=%q digest=%q", result.ImageRef, result.ImageDigest)
	}
}

func TestWarmupPreparesImageDerivedRootFS(t *testing.T) {
	t.Parallel()

	kernelPath := writeWarmupTestFile(t, "kernel")
	preparedPath := writeWarmupTestFile(t, "prepared.ext4")
	snapshotBaseDir := t.TempDir()

	const imageRef = "ghcr.io/buildkite/cleanroom-base/alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	adapter := New()
	adapter.ensurePreparedRootFSFn = func(_ context.Context, ref string) (preparedRootFS, error) {
		if got, want := ref, imageRef; got != want {
			t.Fatalf("unexpected image ref: got %q want %q", got, want)
		}
		return preparedRootFS{
			Ref:    imageRef,
			Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Path:   preparedPath,
			Hit:    true,
		}, nil
	}

	result, err := adapter.Warmup(context.Background(), backend.WarmupRequest{
		ImageRef: imageRef,
		FirecrackerConfig: backend.FirecrackerConfig{
			KernelImagePath: kernelPath,
			Snapshots: backend.SnapshotConfig{
				Driver:  "file",
				BaseDir: snapshotBaseDir,
			},
			MinimumRootFSBytes: 8 << 30,
		},
	})
	if err != nil {
		t.Fatalf("Warmup returned error: %v", err)
	}

	if got, want := result.RootFSPath, preparedPath; got != want {
		t.Fatalf("unexpected rootfs path: got %q want %q", got, want)
	}
	if got, want := result.RootFSStatus, backend.WarmupStatusCached; got != want {
		t.Fatalf("unexpected rootfs status: got %q want %q", got, want)
	}
	if got, want := result.BaseRootFSRef, preparedPath; got != want {
		t.Fatalf("unexpected base rootfs ref: got %q want %q", got, want)
	}
	if got, want := result.ImageRef, imageRef; got != want {
		t.Fatalf("unexpected image ref: got %q want %q", got, want)
	}
	if got, want := result.ImageDigest, "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"; got != want {
		t.Fatalf("unexpected image digest: got %q want %q", got, want)
	}
	if got, want := result.MinimumRootFSBytes, int64(8<<30); got != want {
		t.Fatalf("unexpected minimum rootfs bytes: got %d want %d", got, want)
	}
}

func TestWarmupRejectsMissingRootFSAndImageRef(t *testing.T) {
	t.Parallel()

	kernelPath := writeWarmupTestFile(t, "kernel")
	adapter := New()

	_, err := adapter.Warmup(context.Background(), backend.WarmupRequest{
		FirecrackerConfig: backend.FirecrackerConfig{
			KernelImagePath: kernelPath,
			Snapshots: backend.SnapshotConfig{
				Driver:  "file",
				BaseDir: t.TempDir(),
			},
		},
	})
	if err == nil {
		t.Fatal("expected missing image ref error")
	}
}

func writeWarmupTestFile(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("test"), 0o644); err != nil {
		t.Fatalf("write warmup test file: %v", err)
	}
	return path
}
