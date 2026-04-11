package gateway

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/wolfeidau/content-cache/backend"
	"github.com/wolfeidau/content-cache/download"
	ccgit "github.com/wolfeidau/content-cache/protocol/git"
	ccoci "github.com/wolfeidau/content-cache/protocol/oci"
	"github.com/wolfeidau/content-cache/store"
	"github.com/wolfeidau/content-cache/store/metadb"

	"github.com/buildkite/cleanroom/internal/paths"
)

// ContentCacheConfig configures the embedded content-cache layer.
type ContentCacheConfig struct {
	// StoragePath is the root directory for blob storage and metadata.
	// Defaults to $XDG_CACHE_HOME/cleanroom/content-cache.
	StoragePath string

	// Credentials resolves per-request upstream authorization headers.
	Credentials CredentialProvider

	// GitAllowedHosts optionally restricts which upstream git hosts may be
	// cached. When empty, git handlers are created dynamically for any
	// policy-allowed host.
	GitAllowedHosts []string

	// OCIRegistries maps a request prefix to an upstream registry URL. Entries
	// augment the built-in defaults and are useful for aliases such as
	// {"docker.io": "https://registry-1.docker.io"} or custom symbolic prefixes.
	OCIRegistries map[string]string

	// TagTTL controls how long OCI tag→digest mappings are cached.
	TagTTL time.Duration

	Logger *log.Logger
}

// NewContentCache initialises a content-addressed cache with git and OCI
// protocol handlers backed by a shared CAFS blob store.
func NewContentCache(cfg ContentCacheConfig) (*ContentCache, error) {
	storagePath := cfg.StoragePath
	if storagePath == "" {
		cacheDir, err := paths.CacheBaseDir()
		if err != nil {
			return nil, fmt.Errorf("resolve cache directory: %w", err)
		}
		storagePath = filepath.Join(cacheDir, "content-cache")
	}
	if err := os.MkdirAll(storagePath, 0o755); err != nil {
		return nil, fmt.Errorf("create content-cache storage directory: %w", err)
	}

	logger := slog.Default()
	if cfg.Logger != nil {
		logger = slog.New(cfg.Logger)
	}

	fsBackend, err := backend.NewFilesystem(filepath.Join(storagePath, "blobs"))
	if err != nil {
		return nil, fmt.Errorf("create filesystem backend: %w", err)
	}

	db := metadb.NewBoltDB()
	if err := db.Open(filepath.Join(storagePath, "meta.db")); err != nil {
		return nil, fmt.Errorf("open metadata database: %w", err)
	}

	cafs := store.NewCAFS(fsBackend, store.WithMetaDB(db))
	dl := download.New()

	// Git and OCI need different redirect behavior: git must not follow
	// redirects across policy boundaries, while OCI pulls commonly rely on
	// registry/CDN redirects for blob downloads.
	gitHTTPClient := newGitContentCacheHTTPClient(cfg.Credentials)
	ociHTTPClient := newOCIContentCacheHTTPClient(cfg.Credentials)

	packIdx, err := metadb.NewEnvelopeIndex(db, "git", "pack", 24*time.Hour)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create git pack index: %w", err)
	}
	manifestIdx, err := metadb.NewEnvelopeIndex(db, "oci", "manifest", 24*time.Hour)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create oci manifest index: %w", err)
	}
	blobIdx, err := metadb.NewEnvelopeIndex(db, "oci", "blob", 24*time.Hour)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create oci blob index: %w", err)
	}
	imageIdx, err := metadb.NewEnvelopeIndex(db, "oci", "image", 24*time.Hour)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create oci image index: %w", err)
	}

	gitIndex := ccgit.NewIndex(packIdx)
	ociIndex := ccoci.NewIndex(imageIdx, manifestIdx, blobIdx)
	allowedGitHosts := normalizeAllowedGitHosts(cfg.GitAllowedHosts)
	registryMappings, err := normalizeOCIRegistryMappings(cfg.OCIRegistries)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	tagTTL := cfg.TagTTL
	if tagTTL == 0 {
		tagTTL = 5 * time.Minute
	}

	cache := &ContentCache{
		closer:      db,
		gitHandlers: make(map[string]http.Handler),
		ociHandlers: make(map[string]ociHandlerEntry),
	}
	cache.buildGitHandler = func(host string) (http.Handler, error) {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			return nil, fmt.Errorf("empty git host")
		}
		if len(allowedGitHosts) > 0 {
			if _, ok := allowedGitHosts[host]; !ok {
				return nil, fmt.Errorf("git host %q is not configured for caching", host)
			}
		}
		return ccgit.NewHandler(
			gitIndex,
			cafs,
			ccgit.WithUpstream(ccgit.NewUpstream(
				ccgit.WithHTTPClient(gitHTTPClient),
				ccgit.WithUpstreamLogger(logger),
			)),
			ccgit.WithAllowedHosts([]string{host}),
			ccgit.WithDownloader(dl),
			ccgit.WithLogger(logger),
		), nil
	}
	cache.buildOCIHandler = func(prefix string) (ociHandlerEntry, error) {
		route, err := resolveOCIRegistryRoute(prefix, registryMappings)
		if err != nil {
			return ociHandlerEntry{}, err
		}

		router, err := ccoci.NewRouter([]ccoci.Registry{{
			Prefix: route.prefix,
			Upstream: ccoci.NewUpstream(
				ccoci.WithRegistryURL(route.upstreamURL),
				ccoci.WithHTTPClient(ociHTTPClient),
			),
			TagTTL: tagTTL,
		}}, ccoci.WithRouterLogger(logger))
		if err != nil {
			return ociHandlerEntry{}, fmt.Errorf("create oci router for %q: %w", route.prefix, err)
		}

		handler := ccoci.NewHandler(
			ociIndex,
			cafs,
			ccoci.WithRouter(router),
			ccoci.WithDownloader(dl),
			ccoci.WithTagTTL(tagTTL),
			ccoci.WithLogger(logger),
		)
		entry := ociHandlerEntry{
			handler:      handler,
			policyHost:   route.policyHost,
			upstreamHost: route.upstreamHost,
		}
		if closer, ok := any(handler).(interface{ Close() }); ok {
			entry.closer = closeFunc(closer.Close)
		}
		return entry, nil
	}
	return cache, nil
}

type ociRoute struct {
	prefix       string
	policyHost   string
	upstreamURL  string
	upstreamHost string
}

func normalizeAllowedGitHosts(hosts []string) map[string]struct{} {
	if len(hosts) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			continue
		}
		out[host] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeOCIRegistryMappings(registries map[string]string) (map[string]string, error) {
	out := map[string]string{
		"docker.io":            "https://registry-1.docker.io",
		"index.docker.io":      "https://registry-1.docker.io",
		"registry-1.docker.io": "https://registry-1.docker.io",
	}

	for prefix, registryURL := range registries {
		normalizedPrefix := strings.ToLower(strings.TrimSpace(prefix))
		normalizedURL := strings.TrimRight(strings.TrimSpace(registryURL), "/")
		if normalizedPrefix == "" || normalizedURL == "" {
			continue
		}
		if !strings.Contains(normalizedURL, "://") {
			normalizedURL = "https://" + normalizedURL
		}
		if host := registryHostname(normalizedURL); host == "" {
			return nil, fmt.Errorf("registry mapping for %q has invalid upstream %q", prefix, registryURL)
		}
		out[normalizedPrefix] = normalizedURL
	}
	return out, nil
}

func resolveOCIRegistryRoute(prefix string, registries map[string]string) (ociRoute, error) {
	normalizedPrefix := strings.ToLower(strings.TrimSpace(prefix))
	if normalizedPrefix == "" {
		return ociRoute{}, fmt.Errorf("registry prefix must not be empty")
	}

	if upstreamURL, ok := registries[normalizedPrefix]; ok {
		upstreamHost := registryHostname(upstreamURL)
		policyHost := upstreamHost
		if isRegistryHostPrefix(normalizedPrefix) {
			policyHost = normalizedPrefix
		}
		return ociRoute{
			prefix:       normalizedPrefix,
			policyHost:   policyHost,
			upstreamURL:  upstreamURL,
			upstreamHost: upstreamHost,
		}, nil
	}

	if !isRegistryHostPrefix(normalizedPrefix) {
		return ociRoute{}, fmt.Errorf("unknown registry prefix %q", prefix)
	}

	upstreamURL := "https://" + normalizedPrefix
	return ociRoute{
		prefix:       normalizedPrefix,
		policyHost:   normalizedPrefix,
		upstreamURL:  upstreamURL,
		upstreamHost: registryHostname(upstreamURL),
	}, nil
}
