package imagemgr

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testImageRef = "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestEnsureCachesAndReusesImage(t *testing.T) {
	t.Parallel()

	var pulls int
	manager := newTestManager(t, func(_ context.Context, _ string) (io.ReadCloser, OCIConfig, error) {
		pulls++
		return io.NopCloser(bytes.NewReader(testRootFSTar(t))), OCIConfig{
			Entrypoint: []string{"/bin/sh"},
			Cmd:        []string{"-lc", "echo hello"},
			Workdir:    "/workspace",
		}, nil
	})

	first, err := manager.Ensure(context.Background(), testImageRef)
	if err != nil {
		t.Fatalf("Ensure (first) returned error: %v", err)
	}
	if first.CacheHit {
		t.Fatal("expected first ensure to be a cache miss")
	}
	if _, err := os.Stat(first.Record.RootFSPath); err != nil {
		t.Fatalf("expected rootfs artifact to exist after first ensure: %v", err)
	}

	second, err := manager.Ensure(context.Background(), testImageRef)
	if err != nil {
		t.Fatalf("Ensure (second) returned error: %v", err)
	}
	if !second.CacheHit {
		t.Fatal("expected second ensure to hit cache")
	}
	if pulls != 1 {
		t.Fatalf("expected one registry pull, got %d", pulls)
	}

	items, err := manager.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one cached image, got %d", len(items))
	}
	if got, want := items[0].Digest, "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"; got != want {
		t.Fatalf("unexpected digest: got %q want %q", got, want)
	}
	if got, want := items[0].OCIConfig.Workdir, "/workspace"; got != want {
		t.Fatalf("unexpected OCI workdir: got %q want %q", got, want)
	}
}

func TestImportAndRemoveByDigestSelector(t *testing.T) {
	t.Parallel()

	manager := newTestManager(t, nil)

	importPath := filepath.Join(t.TempDir(), "rootfs.tar")
	if err := os.WriteFile(importPath, testRootFSTar(t), 0o644); err != nil {
		t.Fatalf("write import tarball: %v", err)
	}

	record, err := manager.Import(context.Background(), testImageRef, importPath, nil)
	if err != nil {
		t.Fatalf("Import returned error: %v", err)
	}
	if _, err := os.Stat(record.RootFSPath); err != nil {
		t.Fatalf("expected imported rootfs to exist: %v", err)
	}

	removed, err := manager.Remove(context.Background(), "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("expected one removed image, got %d", len(removed))
	}
	if _, err := os.Stat(record.RootFSPath); !os.IsNotExist(err) {
		t.Fatalf("expected removed rootfs to be deleted, stat err=%v", err)
	}

	items, err := manager.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected empty cache after remove, got %d entries", len(items))
	}
}

func TestRemoveByRefSelector(t *testing.T) {
	t.Parallel()

	manager := newTestManager(t, nil)
	if _, err := manager.Import(context.Background(), testImageRef, "-", bytes.NewReader(testRootFSTar(t))); err != nil {
		t.Fatalf("Import returned error: %v", err)
	}

	removed, err := manager.Remove(context.Background(), testImageRef)
	if err != nil {
		t.Fatalf("Remove by ref returned error: %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("expected one removed image, got %d", len(removed))
	}
}

func newTestManager(t *testing.T, pullFn func(context.Context, string) (io.ReadCloser, OCIConfig, error)) *Manager {
	t.Helper()

	cacheDir := filepath.Join(t.TempDir(), "cache")
	dbPath := filepath.Join(t.TempDir(), "state", "metadata.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("create state dir: %v", err)
	}

	now := time.Unix(1_700_000_000, 0).UTC()
	manager, err := New(Options{
		CacheDir:       cacheDir,
		MetadataDBPath: dbPath,
		Now: func() time.Time {
			return now
		},
		PullImage: pullFn,
		MaterializeRootFS: func(_ context.Context, stream io.Reader, outputPath string) (int64, error) {
			if _, err := io.Copy(io.Discard, stream); err != nil {
				return 0, err
			}
			if err := os.WriteFile(outputPath, []byte("fake-ext4"), 0o644); err != nil {
				return 0, err
			}
			info, err := os.Stat(outputPath)
			if err != nil {
				return 0, err
			}
			return info.Size(), nil
		},
	})
	if err != nil {
		t.Fatalf("create test image manager: %v", err)
	}

	if manager.pullImage == nil {
		manager.pullImage = func(_ context.Context, _ string) (io.ReadCloser, OCIConfig, error) {
			return io.NopCloser(bytes.NewReader(testRootFSTar(t))), OCIConfig{}, nil
		}
	}
	return manager
}

func testRootFSTar(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := []byte("hello rootfs\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: "etc/motd",
		Mode: 0o644,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("write tar payload: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}

	if !strings.Contains(buf.String(), "motd") {
		t.Fatal("expected tar payload to contain motd entry")
	}
	return buf.Bytes()
}
