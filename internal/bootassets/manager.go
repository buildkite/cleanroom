package bootassets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/paths"
)

var ErrNoManagedKernelAsset = errors.New("no managed kernel asset")

type Selector struct {
	Backend string
	GOOS    string
	GOARCH  string
}

type KernelSpec struct {
	ID       string
	Filename string
	URL      string
	SHA256   string
}

type EnsureResult struct {
	Path     string
	CacheHit bool
	Spec     KernelSpec
}

type ResolveResult struct {
	Path     string
	Managed  bool
	CacheHit bool
	Notice   string
	Spec     KernelSpec
}

type Options struct {
	HTTPClient *http.Client
	AssetsDir  func() (string, error)
	Specs      map[Selector]KernelSpec
}

type Manager struct {
	client    *http.Client
	assetsDir func() (string, error)
	specs     map[Selector]KernelSpec
	mu        sync.Mutex
}

func New(opts Options) *Manager {
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	assetsDir := opts.AssetsDir
	if assetsDir == nil {
		assetsDir = paths.AssetsDir
	}
	specs := opts.Specs
	if specs == nil {
		specs = defaultKernelSpecs()
	}

	copied := make(map[Selector]KernelSpec, len(specs))
	for k, v := range specs {
		copied[k] = v
	}

	return &Manager{
		client:    client,
		assetsDir: assetsDir,
		specs:     copied,
	}
}

func defaultKernelSpecs() map[Selector]KernelSpec {
	return map[Selector]KernelSpec{
		{Backend: "firecracker", GOOS: "linux", GOARCH: "amd64"}: {
			ID:       "fc-ci-v1.14-linux-amd64-vmlinux-6.1.155",
			Filename: "vmlinux-6.1.155",
			URL:      "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.14/x86_64/vmlinux-6.1.155",
			SHA256:   "e41c7048bd2475e7e788153823fcb9166a7e0b78c4c443bd6446d015fa735f53",
		},
		{Backend: "firecracker", GOOS: "linux", GOARCH: "arm64"}: {
			ID:       "fc-ci-v1.14-linux-arm64-vmlinux-6.1.155",
			Filename: "vmlinux-6.1.155",
			URL:      "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.14/aarch64/vmlinux-6.1.155",
			SHA256:   "61baeae1ac6197be4fc5c71fa78df266acdc33c54570290d2f611c2b42c105be",
		},
		{Backend: "darwin-vz", GOOS: "darwin", GOARCH: "arm64"}: {
			ID:       "fc-ci-v1.14-darwin-arm64-vmlinux-6.1.155",
			Filename: "vmlinux-6.1.155",
			URL:      "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.14/aarch64/vmlinux-6.1.155",
			SHA256:   "61baeae1ac6197be4fc5c71fa78df266acdc33c54570290d2f611c2b42c105be",
		},
		{Backend: "darwin-vz", GOOS: "darwin", GOARCH: "amd64"}: {
			ID:       "fc-ci-v1.14-darwin-amd64-vmlinux-6.1.155",
			Filename: "vmlinux-6.1.155",
			URL:      "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.14/x86_64/vmlinux-6.1.155",
			SHA256:   "e41c7048bd2475e7e788153823fcb9166a7e0b78c4c443bd6446d015fa735f53",
		},
	}
}

func (m *Manager) Lookup(backendName, goos, goarch string) (KernelSpec, bool) {
	spec, ok := m.specs[Selector{
		Backend: strings.TrimSpace(backendName),
		GOOS:    strings.TrimSpace(goos),
		GOARCH:  strings.TrimSpace(goarch),
	}]
	return spec, ok
}

func (m *Manager) KernelPath(backendName, goos, goarch string) (string, error) {
	spec, ok := m.Lookup(backendName, goos, goarch)
	if !ok {
		return "", fmt.Errorf("%w for backend=%s host=%s/%s", ErrNoManagedKernelAsset, backendName, goos, goarch)
	}
	base, err := m.assetsDir()
	if err != nil {
		return "", fmt.Errorf("resolve assets directory: %w", err)
	}
	return filepath.Join(base, "kernels", spec.ID, spec.Filename), nil
}

func (m *Manager) EnsureKernel(ctx context.Context, backendName, goos, goarch string) (EnsureResult, error) {
	spec, ok := m.Lookup(backendName, goos, goarch)
	if !ok {
		return EnsureResult{}, fmt.Errorf("%w for backend=%s host=%s/%s", ErrNoManagedKernelAsset, backendName, goos, goarch)
	}
	dest, err := m.KernelPath(backendName, goos, goarch)
	if err != nil {
		return EnsureResult{}, err
	}

	valid, err := fileMatchesSHA256(dest, spec.SHA256)
	if err != nil {
		return EnsureResult{}, err
	}
	if valid {
		return EnsureResult{Path: dest, CacheHit: true, Spec: spec}, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	valid, err = fileMatchesSHA256(dest, spec.SHA256)
	if err != nil {
		return EnsureResult{}, err
	}
	if valid {
		return EnsureResult{Path: dest, CacheHit: true, Spec: spec}, nil
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return EnsureResult{}, fmt.Errorf("create kernel asset directory %q: %w", filepath.Dir(dest), err)
	}

	tmp := dest + fmt.Sprintf(".tmp-%d", time.Now().UnixNano())
	if err := m.downloadAndVerify(ctx, spec, tmp); err != nil {
		_ = os.Remove(tmp)
		return EnsureResult{}, err
	}

	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		if valid, vErr := fileMatchesSHA256(dest, spec.SHA256); vErr == nil && valid {
			return EnsureResult{Path: dest, CacheHit: true, Spec: spec}, nil
		}
		return EnsureResult{}, fmt.Errorf("store kernel asset %q: %w", dest, err)
	}

	return EnsureResult{Path: dest, CacheHit: false, Spec: spec}, nil
}

func (m *Manager) ResolveKernelPath(ctx context.Context, backendName, goos, goarch, configuredPath string) (ResolveResult, error) {
	trimmed := strings.TrimSpace(configuredPath)
	if trimmed != "" {
		absPath, err := filepath.Abs(trimmed)
		if err == nil {
			trimmed = absPath
		}
		if st, err := os.Stat(trimmed); err == nil && !st.IsDir() {
			return ResolveResult{Path: trimmed}, nil
		}

		ensured, err := m.EnsureKernel(ctx, backendName, goos, goarch)
		if err != nil {
			return ResolveResult{}, fmt.Errorf("configured kernel_image %q is not accessible and managed kernel resolution failed: %w", trimmed, err)
		}
		return ResolveResult{
			Path:     ensured.Path,
			Managed:  true,
			CacheHit: ensured.CacheHit,
			Spec:     ensured.Spec,
			Notice: fmt.Sprintf(
				"configured kernel_image %q is not accessible; using managed kernel asset %s (%s)",
				trimmed,
				ensured.Spec.ID,
				cacheState(ensured.CacheHit),
			),
		}, nil
	}

	ensured, err := m.EnsureKernel(ctx, backendName, goos, goarch)
	if err != nil {
		return ResolveResult{}, err
	}
	return ResolveResult{
		Path:     ensured.Path,
		Managed:  true,
		CacheHit: ensured.CacheHit,
		Spec:     ensured.Spec,
		Notice:   fmt.Sprintf("using managed kernel asset %s (%s)", ensured.Spec.ID, cacheState(ensured.CacheHit)),
	}, nil
}

func (m *Manager) downloadAndVerify(ctx context.Context, spec KernelSpec, tmpPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.URL, nil)
	if err != nil {
		return fmt.Errorf("create kernel asset request: %w", err)
	}
	req.Header.Set("User-Agent", "cleanroom")

	res, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("download kernel asset from %s: %w", spec.URL, err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		return fmt.Errorf("download kernel asset from %s: unexpected status %d: %s", spec.URL, res.StatusCode, strings.TrimSpace(string(body)))
	}

	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create temporary kernel asset %q: %w", tmpPath, err)
	}
	defer out.Close()

	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, hash), res.Body); err != nil {
		return fmt.Errorf("write kernel asset %q: %w", tmpPath, err)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(got, spec.SHA256) {
		return fmt.Errorf("kernel asset checksum mismatch for %s: got %s want %s", spec.URL, got, spec.SHA256)
	}
	return nil
}

func fileMatchesSHA256(path, wantSHA256 string) (bool, error) {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat kernel asset %q: %w", path, err)
	}
	if st.IsDir() {
		return false, fmt.Errorf("kernel asset path %q is a directory", path)
	}

	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open kernel asset %q: %w", path, err)
	}
	defer f.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return false, fmt.Errorf("hash kernel asset %q: %w", path, err)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	return strings.EqualFold(got, wantSHA256), nil
}

func cacheState(hit bool) string {
	if hit {
		return "cache hit"
	}
	return "cache miss"
}

var defaultManager = New(Options{})

func LookupManagedKernel(backendName, goos, goarch string) (KernelSpec, bool) {
	return defaultManager.Lookup(backendName, goos, goarch)
}

func LookupManagedKernelForHost(backendName string) (KernelSpec, bool) {
	return defaultManager.Lookup(backendName, runtime.GOOS, runtime.GOARCH)
}

func ManagedKernelPath(backendName, goos, goarch string) (string, error) {
	return defaultManager.KernelPath(backendName, goos, goarch)
}

func ManagedKernelPathForHost(backendName string) (string, error) {
	return defaultManager.KernelPath(backendName, runtime.GOOS, runtime.GOARCH)
}

func ResolveKernelPathForHost(ctx context.Context, backendName, configuredPath string) (ResolveResult, error) {
	return defaultManager.ResolveKernelPath(ctx, backendName, runtime.GOOS, runtime.GOARCH, configuredPath)
}
