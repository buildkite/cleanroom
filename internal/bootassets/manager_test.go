package bootassets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
			{Backend: "firecracker", GOOS: "linux", GOARCH: "amd64"}: {
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
			{Backend: "firecracker", GOOS: "linux", GOARCH: "amd64"}: {
				ID:       "test-kernel",
				Filename: "vmlinux-test",
				URL:      srv.URL + "/kernel",
				SHA256:   sha256Hex([]byte(payload)),
			},
		},
	})

	first, err := mgr.ResolveKernelPath(context.Background(), "firecracker", "linux", "amd64", "")
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

	second, err := mgr.ResolveKernelPath(context.Background(), "firecracker", "linux", "amd64", "")
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

	_, err := mgr.ResolveKernelPath(context.Background(), "unknown", "linux", "amd64", "")
	if err == nil {
		t.Fatal("expected unsupported-platform error")
	}
	if !strings.Contains(err.Error(), "no managed kernel asset") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveKernelPathUsesLatestReleaseManifestForDevDarwinVZ(t *testing.T) {
	t.Parallel()

	const payload = "release-kernel"
	var releaseHits atomic.Int32
	var kernelHits atomic.Int32
	srv := newDarwinVZKernelReleaseServer(t, "/repos/buildkite/cleanroom/releases/latest", payload, &releaseHits, &kernelHits)

	tmpDir := t.TempDir()
	mgr := New(Options{
		HTTPClient: srv.Client(),
		AssetsDir: func() (string, error) {
			return filepath.Join(tmpDir, "assets"), nil
		},
		Specs:         map[Selector]KernelSpec{},
		GitHubAPIBase: srv.URL,
	})

	first, err := mgr.ResolveKernelPathWithVersion(context.Background(), "darwin-vz", "darwin", "arm64", "", "dev")
	if err != nil {
		t.Fatalf("ResolveKernelPathWithVersion first call returned error: %v", err)
	}
	if got, want := first.Spec.ID, "cleanroom-darwin-vz-minimal-rootfs-arm64-linux-6.1.155"; got != want {
		t.Fatalf("unexpected release kernel spec ID: got %q want %q", got, want)
	}
	if got, want := filepath.Base(first.Path), "cleanroom-darwin-vz-minimal-rootfs-arm64-linux-6.1.155-Image"; got != want {
		t.Fatalf("unexpected cached release kernel path: got %q want %q", got, want)
	}
	if first.CacheHit {
		t.Fatal("expected first release kernel resolution to be a cache miss")
	}

	second, err := mgr.ResolveKernelPathWithVersion(context.Background(), "darwin-vz", "darwin", "arm64", "", "dev")
	if err != nil {
		t.Fatalf("ResolveKernelPathWithVersion second call returned error: %v", err)
	}
	if !second.CacheHit {
		t.Fatal("expected second release kernel resolution to be a cache hit")
	}
	if got := releaseHits.Load(); got != 1 {
		t.Fatalf("expected one release metadata request, got %d", got)
	}
	if got := kernelHits.Load(); got != 1 {
		t.Fatalf("expected one release kernel download, got %d", got)
	}
}

func TestResolveKernelPathUsesMatchingReleaseManifestForTaggedDarwinVZ(t *testing.T) {
	t.Parallel()

	const payload = "release-kernel"
	var releaseHits atomic.Int32
	var kernelHits atomic.Int32
	srv := newDarwinVZKernelReleaseServer(t, "/repos/buildkite/cleanroom/releases/tags/v1.2.3", payload, &releaseHits, &kernelHits)

	tmpDir := t.TempDir()
	mgr := New(Options{
		HTTPClient: srv.Client(),
		AssetsDir: func() (string, error) {
			return filepath.Join(tmpDir, "assets"), nil
		},
		Specs:         map[Selector]KernelSpec{},
		GitHubAPIBase: srv.URL,
	})

	res, err := mgr.ResolveKernelPathWithVersion(context.Background(), "darwin-vz", "darwin", "arm64", "", "1.2.3")
	if err != nil {
		t.Fatalf("ResolveKernelPathWithVersion returned error: %v", err)
	}
	if !res.Managed {
		t.Fatal("expected release kernel to be managed")
	}
	if got := releaseHits.Load(); got != 1 {
		t.Fatalf("expected one tagged release metadata request, got %d", got)
	}
	if got := kernelHits.Load(); got != 1 {
		t.Fatalf("expected one tagged release kernel download, got %d", got)
	}
}

func TestResolveKernelPathFallsBackToStaticDarwinVZWhenReleaseManifestMissing(t *testing.T) {
	t.Parallel()

	const payload = "static-kernel"
	var releaseHits atomic.Int32
	var kernelHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/buildkite/cleanroom/releases/latest":
			releaseHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tag_name":"v1.2.3","assets":[]}`))
		case "/static-kernel":
			kernelHits.Add(1)
			_, _ = w.Write([]byte(payload))
		default:
			http.NotFound(w, r)
		}
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
				ID:       "static-darwin-vz-kernel",
				Filename: "vmlinux-static",
				URL:      srv.URL + "/static-kernel",
				SHA256:   sha256Hex([]byte(payload)),
			},
		},
		GitHubAPIBase: srv.URL,
	})

	res, err := mgr.ResolveKernelPathWithVersion(context.Background(), "darwin-vz", "darwin", "arm64", "", "dev")
	if err != nil {
		t.Fatalf("ResolveKernelPathWithVersion returned error: %v", err)
	}
	if got, want := res.Spec.ID, "static-darwin-vz-kernel"; got != want {
		t.Fatalf("unexpected fallback kernel spec ID: got %q want %q", got, want)
	}
	if got := releaseHits.Load(); got != 1 {
		t.Fatalf("expected one release metadata request, got %d", got)
	}
	if got := kernelHits.Load(); got != 1 {
		t.Fatalf("expected one static kernel download, got %d", got)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newDarwinVZKernelReleaseServer(t *testing.T, releasePath, payload string, releaseHits, kernelHits *atomic.Int32) *httptest.Server {
	t.Helper()

	const assetBase = "cleanroom-darwin-vz-minimal-rootfs-arm64-linux-6.1.155"
	const imageName = assetBase + "-Image"
	const manifestName = assetBase + ".manifest.json"

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case releasePath:
			releaseHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
  "tag_name": "v1.2.3",
  "assets": [
    {"name": %q, "browser_download_url": %q},
    {"name": %q, "browser_download_url": %q}
  ]
}`, manifestName, srv.URL+"/manifest.json", imageName, srv.URL+"/kernel")
		case "/manifest.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
  "id": %q,
  "backend": "darwin-vz",
  "profile": "rootfs",
  "arch": "arm64",
  "assets": {
    "image": %q,
    "config": %q,
    "sha256": %q,
    "manifest": %q
  },
  "sha256": %q
}`, assetBase, imageName, imageName+".config", imageName+".sha256", manifestName, sha256Hex([]byte(payload)))
		case "/kernel":
			kernelHits.Add(1)
			_, _ = w.Write([]byte(payload))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}
