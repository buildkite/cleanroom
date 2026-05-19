package bootassets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/paths"
)

var ErrNoManagedKernelAsset = errors.New("no managed kernel asset")

const (
	defaultGitHubAPIBase           = "https://api.github.com"
	defaultGitHubRepository        = "buildkite/cleanroom"
	darwinVZReleaseManifestPrefix  = "cleanroom-darwin-vz-minimal-rootfs-arm64-linux-"
	darwinVZReleaseManifestSuffix  = ".manifest.json"
	latestCleanroomReleaseSelector = "latest"
	releaseManifestResolveTimeout  = 15 * time.Second
)

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
	HTTPClient       *http.Client
	AssetsDir        func() (string, error)
	Specs            map[Selector]KernelSpec
	GitHubAPIBase    string
	GitHubRepository string
}

type Manager struct {
	client           *http.Client
	assetsDir        func() (string, error)
	specs            map[Selector]KernelSpec
	githubAPIBase    string
	githubRepository string
	releaseSpecCache map[releaseKernelCacheKey]KernelSpec
	mu               sync.Mutex
}

type releaseKernelCacheKey struct {
	Backend string
	GOOS    string
	GOARCH  string
	Release string
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
	githubAPIBase := strings.TrimRight(strings.TrimSpace(opts.GitHubAPIBase), "/")
	if githubAPIBase == "" {
		githubAPIBase = defaultGitHubAPIBase
	}
	githubRepository := strings.TrimSpace(opts.GitHubRepository)
	if githubRepository == "" {
		githubRepository = defaultGitHubRepository
	}

	return &Manager{
		client:           client,
		assetsDir:        assetsDir,
		specs:            copied,
		githubAPIBase:    githubAPIBase,
		githubRepository: githubRepository,
		releaseSpecCache: make(map[releaseKernelCacheKey]KernelSpec),
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
	return m.kernelPathForSpec(spec)
}

func (m *Manager) kernelPathForSpec(spec KernelSpec) (string, error) {
	base, err := m.assetsDir()
	if err != nil {
		return "", fmt.Errorf("resolve assets directory: %w", err)
	}
	return filepath.Join(base, "kernels", spec.ID, spec.Filename), nil
}

func (m *Manager) EnsureKernel(ctx context.Context, backendName, goos, goarch string) (EnsureResult, error) {
	return m.EnsureKernelWithVersion(ctx, backendName, goos, goarch, "")
}

func (m *Manager) EnsureKernelWithVersion(ctx context.Context, backendName, goos, goarch, appVersion string) (EnsureResult, error) {
	spec, err := m.resolveKernelSpec(ctx, backendName, goos, goarch, appVersion)
	if err != nil {
		return EnsureResult{}, err
	}
	dest, err := m.kernelPathForSpec(spec)
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
	return m.ResolveKernelPathWithVersion(ctx, backendName, goos, goarch, configuredPath, "")
}

func (m *Manager) ResolveKernelPathWithVersion(ctx context.Context, backendName, goos, goarch, configuredPath, appVersion string) (ResolveResult, error) {
	trimmed := strings.TrimSpace(configuredPath)
	if trimmed != "" {
		absPath, err := filepath.Abs(trimmed)
		if err == nil {
			trimmed = absPath
		}
		if st, err := os.Stat(trimmed); err == nil && !st.IsDir() {
			return ResolveResult{Path: trimmed}, nil
		}

		ensured, err := m.EnsureKernelWithVersion(ctx, backendName, goos, goarch, appVersion)
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

	ensured, err := m.EnsureKernelWithVersion(ctx, backendName, goos, goarch, appVersion)
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

func (m *Manager) resolveKernelSpec(ctx context.Context, backendName, goos, goarch, appVersion string) (KernelSpec, error) {
	if spec, ok, err := m.resolveDarwinVZReleaseKernelSpec(ctx, backendName, goos, goarch, appVersion); ok && err == nil {
		return spec, nil
	}

	if spec, ok := m.Lookup(backendName, goos, goarch); ok {
		return spec, nil
	}
	return KernelSpec{}, fmt.Errorf("%w for backend=%s host=%s/%s", ErrNoManagedKernelAsset, backendName, goos, goarch)
}

func (m *Manager) resolveDarwinVZReleaseKernelSpec(ctx context.Context, backendName, goos, goarch, appVersion string) (KernelSpec, bool, error) {
	backendName = strings.TrimSpace(backendName)
	goos = strings.TrimSpace(goos)
	goarch = strings.TrimSpace(goarch)
	if backendName != "darwin-vz" || goos != "darwin" || goarch != "arm64" {
		return KernelSpec{}, false, nil
	}

	release := cleanroomKernelReleaseSelector(appVersion)
	cacheKey := releaseKernelCacheKey{
		Backend: backendName,
		GOOS:    goos,
		GOARCH:  goarch,
		Release: release,
	}
	m.mu.Lock()
	if spec, ok := m.releaseSpecCache[cacheKey]; ok {
		m.mu.Unlock()
		return spec, true, nil
	}
	m.mu.Unlock()

	spec, err := m.fetchDarwinVZReleaseKernelSpec(ctx, release)
	if err != nil {
		return KernelSpec{}, true, err
	}

	m.mu.Lock()
	m.releaseSpecCache[cacheKey] = spec
	m.mu.Unlock()
	return spec, true, nil
}

type githubRelease struct {
	TagName string               `json:"tag_name"`
	Assets  []githubReleaseAsset `json:"assets"`
}

type githubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type kernelReleaseManifest struct {
	ID      string `json:"id"`
	Backend string `json:"backend"`
	Profile string `json:"profile"`
	Arch    string `json:"arch"`
	Assets  struct {
		Image    string `json:"image"`
		Config   string `json:"config"`
		SHA256   string `json:"sha256"`
		Manifest string `json:"manifest"`
	} `json:"assets"`
	SHA256 string `json:"sha256"`
}

func (m *Manager) fetchDarwinVZReleaseKernelSpec(ctx context.Context, releaseSelector string) (KernelSpec, error) {
	metadataCtx, cancel := context.WithTimeout(ctx, releaseManifestResolveTimeout)
	defer cancel()

	releaseURL := m.releaseURL(releaseSelector)
	var release githubRelease
	if err := m.downloadJSON(metadataCtx, releaseURL, &release); err != nil {
		return KernelSpec{}, err
	}

	manifestAsset, ok := findDarwinVZKernelManifestAsset(release.Assets)
	if !ok {
		return KernelSpec{}, fmt.Errorf("darwin-vz kernel manifest asset not found in Cleanroom release %s", releaseSelector)
	}

	var manifest kernelReleaseManifest
	if err := m.downloadJSON(metadataCtx, manifestAsset.BrowserDownloadURL, &manifest); err != nil {
		return KernelSpec{}, fmt.Errorf("download darwin-vz kernel manifest %q: %w", manifestAsset.Name, err)
	}
	if err := validateDarwinVZKernelManifest(manifest); err != nil {
		return KernelSpec{}, fmt.Errorf("invalid darwin-vz kernel manifest %q: %w", manifestAsset.Name, err)
	}

	imageAsset, ok := findReleaseAsset(release.Assets, manifest.Assets.Image)
	if !ok {
		return KernelSpec{}, fmt.Errorf("darwin-vz kernel image asset %q not found in Cleanroom release %s", manifest.Assets.Image, releaseSelector)
	}
	return KernelSpec{
		ID:       manifest.ID,
		Filename: manifest.Assets.Image,
		URL:      imageAsset.BrowserDownloadURL,
		SHA256:   manifest.SHA256,
	}, nil
}

func (m *Manager) releaseURL(releaseSelector string) string {
	if releaseSelector == latestCleanroomReleaseSelector {
		return fmt.Sprintf("%s/repos/%s/releases/latest", m.githubAPIBase, m.githubRepository)
	}
	return fmt.Sprintf("%s/repos/%s/releases/tags/%s", m.githubAPIBase, m.githubRepository, neturl.PathEscape(releaseSelector))
}

func (m *Manager) downloadJSON(ctx context.Context, rawURL string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "cleanroom")
	req.Header.Set("Accept", "application/vnd.github+json, application/json")

	res, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", rawURL, err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		return fmt.Errorf("download %s: unexpected status %d: %s", rawURL, res.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", rawURL, err)
	}
	return nil
}

func findDarwinVZKernelManifestAsset(assets []githubReleaseAsset) (githubReleaseAsset, bool) {
	for _, asset := range assets {
		if strings.HasPrefix(asset.Name, darwinVZReleaseManifestPrefix) && strings.HasSuffix(asset.Name, darwinVZReleaseManifestSuffix) {
			return asset, strings.TrimSpace(asset.BrowserDownloadURL) != ""
		}
	}
	return githubReleaseAsset{}, false
}

func findReleaseAsset(assets []githubReleaseAsset, name string) (githubReleaseAsset, bool) {
	for _, asset := range assets {
		if asset.Name == name {
			return asset, strings.TrimSpace(asset.BrowserDownloadURL) != ""
		}
	}
	return githubReleaseAsset{}, false
}

func validateDarwinVZKernelManifest(manifest kernelReleaseManifest) error {
	switch {
	case strings.TrimSpace(manifest.ID) == "":
		return errors.New("missing id")
	case manifest.Backend != "darwin-vz":
		return fmt.Errorf("unexpected backend %q", manifest.Backend)
	case manifest.Profile != "rootfs":
		return fmt.Errorf("unexpected profile %q", manifest.Profile)
	case manifest.Arch != "arm64":
		return fmt.Errorf("unexpected arch %q", manifest.Arch)
	case strings.TrimSpace(manifest.Assets.Image) == "":
		return errors.New("missing image asset name")
	case strings.TrimSpace(manifest.SHA256) == "":
		return errors.New("missing image sha256")
	default:
		return nil
	}
}

func cleanroomKernelReleaseSelector(version string) string {
	version = strings.TrimSpace(version)
	if isCleanroomReleaseVersion(version) {
		if strings.HasPrefix(version, "v") {
			return version
		}
		return "v" + version
	}
	return latestCleanroomReleaseSelector
}

func isCleanroomReleaseVersion(version string) bool {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
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

func ResolveKernelPathForHostWithVersion(ctx context.Context, backendName, configuredPath, appVersion string) (ResolveResult, error) {
	return defaultManager.ResolveKernelPathWithVersion(ctx, backendName, runtime.GOOS, runtime.GOARCH, configuredPath, appVersion)
}
