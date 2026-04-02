package gateway

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

	// GitAllowedHosts restricts which upstream git hosts may be proxied.
	GitAllowedHosts []string

	// OCIRegistries maps a URL-prefix to an upstream registry URL.
	// Example: {"docker-hub": "https://registry-1.docker.io", "ghcr": "https://ghcr.io"}
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

	// HTTP client with per-request credential injection.
	httpClient := &http.Client{
		Transport: &credentialInjector{
			base: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				DisableKeepAlives:     true,
			},
			credentials: cfg.Credentials,
		},
	}

	// --- Git handler ---
	var gitHandler http.Handler
	if len(cfg.GitAllowedHosts) > 0 {
		packIdx, err := metadb.NewEnvelopeIndex(db, "git", "pack", 24*time.Hour)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("create git pack index: %w", err)
		}
		gitHandler = ccgit.NewHandler(
			ccgit.NewIndex(packIdx),
			cafs,
			ccgit.WithUpstream(ccgit.NewUpstream(
				ccgit.WithHTTPClient(httpClient),
				ccgit.WithUpstreamLogger(logger),
			)),
			ccgit.WithAllowedHosts(cfg.GitAllowedHosts),
			ccgit.WithDownloader(dl),
			ccgit.WithLogger(logger),
		)
	}

	// --- OCI handler ---
	var ociHandler http.Handler
	prefixHosts := make(map[string]string, len(cfg.OCIRegistries))
	if len(cfg.OCIRegistries) > 0 {
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

		registries := make([]ccoci.Registry, 0, len(cfg.OCIRegistries))
		for prefix, registryURL := range cfg.OCIRegistries {
			registries = append(registries, ccoci.Registry{
				Prefix: prefix,
				Upstream: ccoci.NewUpstream(
					ccoci.WithRegistryURL(registryURL),
					ccoci.WithHTTPClient(httpClient),
				),
			})
			prefixHosts[prefix] = registryHostname(registryURL)
		}
		router, err := ccoci.NewRouter(registries, ccoci.WithRouterLogger(logger))
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("create oci router: %w", err)
		}

		tagTTL := cfg.TagTTL
		if tagTTL == 0 {
			tagTTL = 5 * time.Minute
		}

		ociHandler = ccoci.NewHandler(
			ccoci.NewIndex(imageIdx, manifestIdx, blobIdx),
			cafs,
			ccoci.WithRouter(router),
			ccoci.WithDownloader(dl),
			ccoci.WithTagTTL(tagTTL),
			ccoci.WithLogger(logger),
		)
	}

	return &ContentCache{
		closer:      db,
		gitHandler:  gitHandler,
		ociHandler:  ociHandler,
		prefixHosts: prefixHosts,
	}, nil
}
