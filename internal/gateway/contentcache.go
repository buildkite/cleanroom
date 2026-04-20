package gateway

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/buildkite/content-cache/backend"
	"github.com/buildkite/content-cache/download"
	ccfetch "github.com/buildkite/content-cache/protocol/fetch"
	ccgit "github.com/buildkite/content-cache/protocol/git"
	ccgoproxy "github.com/buildkite/content-cache/protocol/goproxy"
	ccoci "github.com/buildkite/content-cache/protocol/oci"
	ccrubygems "github.com/buildkite/content-cache/protocol/rubygems"
	"github.com/buildkite/content-cache/store"
	"github.com/buildkite/content-cache/store/metadb"
	"github.com/charmbracelet/log"

	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/policy"
)

var errGitHostNotConfiguredForCaching = errors.New("git host not configured for caching")

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

	// RubyGemsUpstreamURL overrides the upstream RubyGems registry URL.
	// Defaults to https://rubygems.org.
	RubyGemsUpstreamURL string

	// RubyGemsMetadataTTL controls how long RubyGems metadata is cached.
	RubyGemsMetadataTTL time.Duration

	// FetchAllowedHosts restricts which upstream hosts may be served by the
	// immutable artifact fetch route.
	FetchAllowedHosts []string

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
	sumDBHTTPClient := newSumDBContentCacheHTTPClient()
	ociHTTPClient := newOCIContentCacheHTTPClient(cfg.Credentials)
	rubyGemsHTTPClient := newRubyGemsContentCacheHTTPClient(cfg.Credentials)
	fetchHTTPClient := newFetchContentCacheHTTPClient()

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
	goProxyModIdx, err := metadb.NewEnvelopeIndex(db, "goproxy", "mod", 24*time.Hour)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create goproxy mod index: %w", err)
	}
	goProxyInfoIdx, err := metadb.NewEnvelopeIndex(db, "goproxy", "info", 24*time.Hour)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create goproxy info index: %w", err)
	}
	goProxyListIdx, err := metadb.NewEnvelopeIndex(db, "goproxy", "list", 24*time.Hour)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create goproxy list index: %w", err)
	}
	sumDBIdx, err := metadb.NewEnvelopeIndex(db, "sumdb", "cache", 0)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create sumdb cache index: %w", err)
	}
	rubyGemsMetadataTTL := cfg.RubyGemsMetadataTTL
	if rubyGemsMetadataTTL == 0 {
		rubyGemsMetadataTTL = 5 * time.Minute
	}
	rubyGemsVersionsIdx, err := metadb.NewEnvelopeIndex(db, "rubygems", "versions", rubyGemsMetadataTTL)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create rubygems versions index: %w", err)
	}
	rubyGemsInfoIdx, err := metadb.NewEnvelopeIndex(db, "rubygems", "info", rubyGemsMetadataTTL)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create rubygems info index: %w", err)
	}
	rubyGemsSpecsIdx, err := metadb.NewEnvelopeIndex(db, "rubygems", "specs", rubyGemsMetadataTTL)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create rubygems specs index: %w", err)
	}
	rubyGemsGemIdx, err := metadb.NewEnvelopeIndex(db, "rubygems", "gem", 24*time.Hour)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create rubygems gem index: %w", err)
	}
	rubyGemsGemspecIdx, err := metadb.NewEnvelopeIndex(db, "rubygems", "gemspec", 24*time.Hour)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create rubygems gemspec index: %w", err)
	}
	fetchResourceIdx, err := metadb.NewEnvelopeIndex(db, "fetch", "resource", 24*time.Hour)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create fetch resource index: %w", err)
	}

	gitIndex := ccgit.NewIndex(packIdx)
	goProxyIndex := ccgoproxy.NewIndex(goProxyModIdx, goProxyInfoIdx, goProxyListIdx)
	sumDBIndex := ccgoproxy.NewSumdbIndex(sumDBIdx)
	ociIndex := ccoci.NewIndex(imageIdx, manifestIdx, blobIdx)
	rubyGemsIndex := ccrubygems.NewIndex(
		rubyGemsVersionsIdx,
		rubyGemsInfoIdx,
		rubyGemsSpecsIdx,
		rubyGemsGemIdx,
		rubyGemsGemspecIdx,
		cafs,
	)
	fetchIndex := ccfetch.NewIndex(fetchResourceIdx)
	allowedGitHosts := normalizeAllowedGitHosts(cfg.GitAllowedHosts)
	allowedFetchHosts := normalizeAllowedHostList(cfg.FetchAllowedHosts)
	allowedFetchHostSet := normalizeAllowedGitHosts(allowedFetchHosts)
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
	goProxyUpstreamURL := strings.TrimSpace(ccgoproxy.DefaultUpstreamURL)
	goProxyPolicyHost, goProxyPolicyPort, err := registryHostPort(goProxyUpstreamURL)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("resolve goproxy upstream: %w", err)
	}
	cache.goProxy = goProxyHandlerEntry{
		policyHost:   goProxyPolicyHost,
		policyPort:   goProxyPolicyPort,
		upstreamHost: goProxyPolicyHost,
		upstreamPort: goProxyPolicyPort,
		handlers:     make(map[string]goProxyScopedHandler),
		maxHandlers:  defaultMaxGoProxyScopedHandlers,
		buildHandler: func(compiled *policy.CompiledPolicy) (goProxyScopedHandler, error) {
			goProxyUpstream := ccgoproxy.NewUpstream(
				ccgoproxy.WithUpstreamURL(goProxyUpstreamURL),
				ccgoproxy.WithHTTPClient(newGoProxyContentCacheHTTPClient(compiled)),
			)
			handler := ccgoproxy.NewHandler(
				goProxyIndex,
				cafs,
				ccgoproxy.WithUpstream(goProxyUpstream),
				ccgoproxy.WithLogger(logger),
				ccgoproxy.WithDownloader(dl),
			)
			entry := goProxyScopedHandler{handler: handler}
			if closer, ok := any(handler).(interface{ Close() }); ok {
				entry.closer = closeFunc(closer.Close)
			}
			return entry, nil
		},
	}

	sumDBUpstreamURL := strings.TrimSpace(ccgoproxy.DefaultSumDBURL)
	sumDBPolicyHost, sumDBPolicyPort, err := registryHostPort(sumDBUpstreamURL)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("resolve sumdb upstream: %w", err)
	}
	sumDBUpstream := ccgoproxy.NewSumdbUpstream(
		ccgoproxy.WithSumdbHTTPClient(sumDBHTTPClient),
	)
	sumDBHandler := ccgoproxy.NewSumdbHandler(
		sumDBIndex,
		cafs,
		ccgoproxy.WithSumdbUpstream(sumDBUpstream),
		ccgoproxy.WithSumdbLogger(logger),
		ccgoproxy.WithSumdbName(ccgoproxy.DefaultSumDBName),
	)
	cache.sumdb = sumDBHandlerEntry{
		handler:      sumDBHandler,
		name:         ccgoproxy.DefaultSumDBName,
		policyHost:   sumDBPolicyHost,
		policyPort:   sumDBPolicyPort,
		upstreamHost: sumDBPolicyHost,
		upstreamPort: sumDBPolicyPort,
	}
	if closer, ok := any(sumDBHandler).(interface{ Close() }); ok {
		cache.sumdb.closer = closeFunc(closer.Close)
	}

	cache.resolveOCIRoute = func(prefix string) (ociRoute, error) {
		return resolveOCIRegistryRoute(prefix, registryMappings)
	}
	cache.buildGitHandler = func(host string) (http.Handler, error) {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			return nil, fmt.Errorf("empty git host")
		}
		if len(allowedGitHosts) > 0 {
			if _, ok := allowedGitHosts[host]; !ok {
				return nil, fmt.Errorf("%w: %s", errGitHostNotConfiguredForCaching, host)
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
		route, err := cache.resolveOCIRoute(prefix)
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
			policyPort:   route.policyPort,
			upstreamHost: route.upstreamHost,
			upstreamPort: route.upstreamPort,
		}
		if closer, ok := any(handler).(interface{ Close() }); ok {
			entry.closer = closeFunc(closer.Close)
		}
		return entry, nil
	}

	rubyGemsUpstreamURL := strings.TrimSpace(cfg.RubyGemsUpstreamURL)
	if rubyGemsUpstreamURL == "" {
		rubyGemsUpstreamURL = ccrubygems.DefaultUpstreamURL
	}
	rubyGemsPolicyHost, rubyGemsPolicyPort, err := registryHostPort(rubyGemsUpstreamURL)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("resolve rubygems upstream: %w", err)
	}
	rubyGemsUpstream := ccrubygems.NewUpstream(
		ccrubygems.WithRegistryURL(rubyGemsUpstreamURL),
		ccrubygems.WithHTTPClient(rubyGemsHTTPClient),
	)
	rubyGemsHandler := ccrubygems.NewHandler(
		rubyGemsIndex,
		cafs,
		ccrubygems.WithUpstream(rubyGemsUpstream),
		ccrubygems.WithLogger(logger),
		ccrubygems.WithDownloader(dl),
		ccrubygems.WithMetadataTTL(rubyGemsMetadataTTL),
	)
	cache.rubyGems = rubyGemsHandlerEntry{
		handler:      rubyGemsHandler,
		policyHost:   rubyGemsPolicyHost,
		policyPort:   rubyGemsPolicyPort,
		upstreamHost: rubyGemsPolicyHost,
		upstreamPort: rubyGemsPolicyPort,
	}
	if closer, ok := any(rubyGemsHandler).(interface{ Close() }); ok {
		cache.rubyGems.closer = closeFunc(closer.Close)
	}
	if len(allowedFetchHosts) > 0 {
		fetchHandler := ccfetch.NewHandler(
			fetchIndex,
			cafs,
			ccfetch.WithHTTPClient(fetchHTTPClient),
			ccfetch.WithLogger(logger),
			ccfetch.WithDownloader(dl),
			ccfetch.WithAllowedHosts(allowedFetchHosts),
		)
		cache.fetch = fetchHandlerEntry{
			handler:      fetchHandler,
			allowedHosts: allowedFetchHostSet,
		}
		if closer, ok := any(fetchHandler).(interface{ Close() }); ok {
			cache.fetch.closer = closeFunc(closer.Close)
		}
	}
	return cache, nil
}

type ociRoute struct {
	prefix       string
	policyHost   string
	policyPort   int
	upstreamURL  string
	upstreamHost string
	upstreamPort int
}

func normalizeAllowedGitHosts(hosts []string) map[string]struct{} {
	if len(hosts) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(hosts))
	for _, host := range normalizeAllowedHostList(hosts) {
		out[host] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeAllowedHostList(hosts []string) []string {
	if len(hosts) == 0 {
		return nil
	}
	out := make([]string, 0, len(hosts))
	seen := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
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
		if host, _, err := registryHostPort(normalizedURL); err != nil || host == "" {
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
		upstreamHost, upstreamPort, err := registryHostPort(upstreamURL)
		if err != nil {
			return ociRoute{}, fmt.Errorf("invalid registry mapping for %q: %w", prefix, err)
		}
		policyHost := upstreamHost
		policyPort := upstreamPort
		if isRegistryHostPrefix(normalizedPrefix) {
			policyHost, policyPort, err = registryHostPort(normalizedPrefix)
			if err != nil {
				return ociRoute{}, fmt.Errorf("invalid registry prefix %q: %w", prefix, err)
			}
		}
		return ociRoute{
			prefix:       normalizedPrefix,
			policyHost:   policyHost,
			policyPort:   policyPort,
			upstreamURL:  upstreamURL,
			upstreamHost: upstreamHost,
			upstreamPort: upstreamPort,
		}, nil
	}

	if !isRegistryHostPrefix(normalizedPrefix) {
		return ociRoute{}, fmt.Errorf("unknown registry prefix %q", prefix)
	}

	upstreamURL := "https://" + normalizedPrefix
	upstreamHost, upstreamPort, err := registryHostPort(upstreamURL)
	if err != nil {
		return ociRoute{}, fmt.Errorf("invalid registry prefix %q: %w", prefix, err)
	}
	return ociRoute{
		prefix:       normalizedPrefix,
		policyHost:   upstreamHost,
		policyPort:   upstreamPort,
		upstreamURL:  upstreamURL,
		upstreamHost: upstreamHost,
		upstreamPort: upstreamPort,
	}, nil
}
