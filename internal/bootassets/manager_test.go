package bootassets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResolveKernelPathUsesConfiguredPathWhenPresent(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("remote-kernel"))
	}))
	t.Cleanup(srv.Close)

	tmpDir := t.TempDir()
	configured := filepath.Join(tmpDir, "configured-kernel")
	if err := os.WriteFile(configured, []byte("local"), 0o644); err != nil {
		t.Fatalf("write configured kernel: %v", err)
	}

	mgr := New(Options{
		HTTPClient: srv.Client(),
		AssetsDir: func() (string, error) {
			return filepath.Join(tmpDir, "assets"), nil
		},
		Specs: map[Selector]KernelSpec{
			{Backend: "darwin-vz", GOOS: "darwin", GOARCH: "arm64"}: {
				ID:       "test-kernel",
				Filename: "vmlinux-test",
				URL:      srv.URL + "/kernel",
				SHA256:   sha256Hex([]byte("remote-kernel")),
			},
		},
	})

	got, err := mgr.ResolveKernelPath(context.Background(), "darwin-vz", "darwin", "arm64", configured)
	if err != nil {
		t.Fatalf("ResolveKernelPath returned error: %v", err)
	}
	if got.Path != configured {
		t.Fatalf("unexpected path: got %q want %q", got.Path, configured)
	}
	if got.Managed {
		t.Fatal("expected configured path to not be managed")
	}
	if hits.Load() != 0 {
		t.Fatalf("expected no network access, got %d hits", hits.Load())
	}
}

func TestResolveKernelPathDownloadsAndCachesManagedKernel(t *testing.T) {
	t.Parallel()

	const payload = "remote-kernel"
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)

	tmpDir := t.TempDir()
	mgr := New(Options{
		HTTPClient: srv.Client(),
		AssetsDir: func() (string, error) {
			return filepath.Join(tmpDir, "assets"), nil
		},
		Specs: map[Selector]KernelSpec{
			{Backend: "darwin-vz", GOOS: "darwin", GOARCH: "arm64"}: {
				ID:       "test-kernel",
				Filename: "vmlinux-test",
				URL:      srv.URL + "/kernel",
				SHA256:   sha256Hex([]byte(payload)),
			},
		},
	})

	first, err := mgr.ResolveKernelPath(context.Background(), "darwin-vz", "darwin", "arm64", "")
	if err != nil {
		t.Fatalf("ResolveKernelPath first call returned error: %v", err)
	}
	if !first.Managed {
		t.Fatal("expected first call to use managed kernel")
	}
	if first.CacheHit {
		t.Fatal("expected first call to be cache miss")
	}
	if !strings.Contains(first.Notice, "managed kernel") {
		t.Fatalf("expected managed notice, got %q", first.Notice)
	}

	second, err := mgr.ResolveKernelPath(context.Background(), "darwin-vz", "darwin", "arm64", "")
	if err != nil {
		t.Fatalf("ResolveKernelPath second call returned error: %v", err)
	}
	if !second.Managed {
		t.Fatal("expected second call to use managed kernel")
	}
	if !second.CacheHit {
		t.Fatal("expected second call to be cache hit")
	}

	if got := hits.Load(); got != 1 {
		t.Fatalf("expected one download, got %d", got)
	}
}

func TestResolveKernelPathFallsBackFromMissingConfiguredPath(t *testing.T) {
	t.Parallel()

	const payload = "remote-kernel"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)

	tmpDir := t.TempDir()
	mgr := New(Options{
		HTTPClient: srv.Client(),
		AssetsDir: func() (string, error) {
			return filepath.Join(tmpDir, "assets"), nil
		},
		Specs: map[Selector]KernelSpec{
			{Backend: "firecracker", GOOS: "linux", GOARCH: "amd64"}: {
				ID:       "test-kernel",
				Filename: "vmlinux-test",
				URL:      srv.URL + "/kernel",
				SHA256:   sha256Hex([]byte(payload)),
			},
		},
	})

	res, err := mgr.ResolveKernelPath(context.Background(), "firecracker", "linux", "amd64", "/tmp/missing-kernel")
	if err != nil {
		t.Fatalf("ResolveKernelPath returned error: %v", err)
	}
	if !res.Managed {
		t.Fatal("expected managed fallback")
	}
	if !strings.Contains(res.Notice, "configured kernel_image") {
		t.Fatalf("expected fallback notice, got %q", res.Notice)
	}
}

func TestResolveKernelPathReturnsErrorWhenUnsupported(t *testing.T) {
	t.Parallel()

	mgr := New(Options{
		AssetsDir: func() (string, error) {
			return t.TempDir(), nil
		},
		Specs: map[Selector]KernelSpec{},
	})

	_, err := mgr.ResolveKernelPath(context.Background(), "darwin-vz", "darwin", "arm64", "")
	if err == nil {
		t.Fatal("expected unsupported-platform error")
	}
	if !strings.Contains(err.Error(), "no managed kernel asset") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
