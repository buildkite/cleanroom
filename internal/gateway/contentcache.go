package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/buildkite/content-cache/backend"
	"github.com/buildkite/content-cache/download"
	ccfetch "github.com/buildkite/content-cache/protocol/fetch"
	ccgit "github.com/buildkite/content-cache/protocol/git"
	ccgoproxy "github.com/buildkite/content-cache/protocol/goproxy"
	ccoci "github.com/buildkite/content-cache/protocol/oci"
	ccrubygems "github.com/buildkite/content-cache/protocol/rubygems"
	"github.com/buildkite/content-cache/store"
	"github.com/buildkite/content-cache/store/metadb"

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

	// OCIRegistries maps a registry host-style prefix to an upstream registry
	// URL. Entries augment the built-in defaults and are useful for host-based
	// remaps such as {"docker.io": "https://registry-1.docker.io"} or
	// {"registry.internal:5000": "https://registry.internal"}.
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

	gitIndex := ccgit.NewIndex(packIdx)
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
		closer:         db,
		gitHandlers:    make(map[string]http.Handler),
		ociHandlers:    make(map[string]*ociHandlerEntry),
		maxOCIHandlers: defaultMaxOCIHandlers,
		ociMirrorHosts: ociMirrorHosts(cfg.OCIRegistries),
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
		handlers:     make(map[string]*goProxyScopedHandler),
		maxHandlers:  defaultMaxGoProxyScopedHandlers,
		buildHandler: func(compiled *policy.CompiledPolicy) (goProxyScopedHandler, error) {
			policyPrefix := scopedMetadataPrefix(compiledPolicyCacheKey(compiled))
			goProxyIndex, err := newScopedGoProxyIndex(db, policyPrefix)
			if err != nil {
				return goProxyScopedHandler{}, err
			}
			goProxyUpstream := ccgoproxy.NewUpstream(
				ccgoproxy.WithUpstreamURL(goProxyUpstreamURL),
				ccgoproxy.WithHTTPClient(newGoProxyContentCacheHTTPClient(compiled)),
			)
			handler := ccgoproxy.NewHandler(
				goProxyIndex,
				cafs,
				ccgoproxy.WithUpstream(goProxyUpstream),
				ccgoproxy.WithLogger(logger),
				ccgoproxy.WithDownloader(download.New()),
			)
			entry := goProxyScopedHandler{handler: handler}
			if closer, ok := any(handler).(interface{ Close() }); ok {
				entry.closer = closeFunc(closer.Close)
			}
			entry.evictCloser = closeFunc(func() {
				if entry.closer != nil {
					_ = entry.closer.Close()
				}
				_ = deleteScopedEnvelopeEntries(context.Background(), db, "goproxy", []string{"mod", "info", "list"}, policyPrefix)
			})
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
			ccgit.WithUpstream(newGitContentCacheUpstream(gitHTTPClient, logger, cfg.Credentials)),
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
		cache.fetch = fetchHandlerEntry{
			allowedHosts: allowedFetchHostSet,
			handlers:     make(map[string]*fetchScopedHandler),
			maxHandlers:  defaultMaxFetchScopedHandlers,
			buildHandler: func(compiled *policy.CompiledPolicy) (fetchScopedHandler, error) {
				policyPrefix := scopedMetadataPrefix(compiledPolicyCacheKey(compiled))
				fetchIndex, err := newScopedFetchIndex(db, policyPrefix)
				if err != nil {
					return fetchScopedHandler{}, err
				}
				handler := ccfetch.NewHandler(
					fetchIndex,
					cafs,
					ccfetch.WithHTTPClient(newFetchContentCacheHTTPClient(compiled)),
					ccfetch.WithLogger(logger),
					ccfetch.WithDownloader(download.New()),
					ccfetch.WithAllowedHosts(allowedFetchHosts),
				)
				entry := fetchScopedHandler{handler: handler}
				if closer, ok := any(handler).(interface{ Close() }); ok {
					entry.closer = closeFunc(closer.Close)
				}
				entry.evictCloser = closeFunc(func() {
					if entry.closer != nil {
						_ = entry.closer.Close()
					}
					_ = deleteScopedEnvelopeEntries(context.Background(), db, "fetch", []string{"resource"}, policyPrefix)
				})
				return entry, nil
			},
		}
	}
	return cache, nil
}

func newScopedGoProxyIndex(db metadb.EnvelopeStore, policyPrefix string) (*ccgoproxy.Index, error) {
	modIdx, err := newScopedEnvelopeIndex(db, "goproxy", "mod", policyPrefix, 24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("create scoped goproxy mod index: %w", err)
	}
	infoIdx, err := newScopedEnvelopeIndex(db, "goproxy", "info", policyPrefix, 24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("create scoped goproxy info index: %w", err)
	}
	listIdx, err := newScopedEnvelopeIndex(db, "goproxy", "list", policyPrefix, 24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("create scoped goproxy list index: %w", err)
	}
	return ccgoproxy.NewIndex(modIdx, infoIdx, listIdx), nil
}

func newScopedFetchIndex(db metadb.EnvelopeStore, policyPrefix string) (*ccfetch.Index, error) {
	resourceIdx, err := newScopedEnvelopeIndex(db, "fetch", "resource", policyPrefix, 24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("create scoped fetch resource index: %w", err)
	}
	return ccfetch.NewIndex(resourceIdx), nil
}

func newScopedEnvelopeIndex(db metadb.EnvelopeStore, protocol, kind, policyPrefix string, ttl time.Duration) (*metadb.EnvelopeIndex, error) {
	return metadb.NewEnvelopeIndex(&scopedEnvelopeStore{base: db, keyPrefix: policyPrefix}, protocol, kind, ttl)
}

func scopedMetadataPrefix(policyKey string) string {
	digest := sha256.Sum256([]byte(policyKey))
	return "policy/" + hex.EncodeToString(digest[:]) + "/"
}

func deleteScopedEnvelopeEntries(ctx context.Context, db metadb.EnvelopeStore, protocol string, kinds []string, policyPrefix string) error {
	var errs []error
	for _, kind := range kinds {
		keys, err := db.ListEnvelopeKeys(ctx, protocol, kind)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, key := range keys {
			if !strings.HasPrefix(key, policyPrefix) {
				continue
			}
			if err := db.DeleteEnvelope(ctx, protocol, kind, key); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

type scopedEnvelopeStore struct {
	base      metadb.EnvelopeStore
	keyPrefix string
}

func (s *scopedEnvelopeStore) scopedKey(key string) string {
	return s.keyPrefix + key
}

func (s *scopedEnvelopeStore) PutEnvelope(ctx context.Context, protocol, kind, key string, env *metadb.MetadataEnvelope) error {
	return s.base.PutEnvelope(ctx, protocol, kind, s.scopedKey(key), env)
}

func (s *scopedEnvelopeStore) GetEnvelope(ctx context.Context, protocol, kind, key string) (*metadb.MetadataEnvelope, error) {
	return s.base.GetEnvelope(ctx, protocol, kind, s.scopedKey(key))
}

func (s *scopedEnvelopeStore) DeleteEnvelope(ctx context.Context, protocol, kind, key string) error {
	return s.base.DeleteEnvelope(ctx, protocol, kind, s.scopedKey(key))
}

func (s *scopedEnvelopeStore) ListEnvelopeKeys(ctx context.Context, protocol, kind string) ([]string, error) {
	keys, err := s.base.ListEnvelopeKeys(ctx, protocol, kind)
	if err != nil {
		return nil, err
	}
	scoped := make([]string, 0, len(keys))
	for _, key := range keys {
		trimmed, ok := strings.CutPrefix(key, s.keyPrefix)
		if ok {
			scoped = append(scoped, trimmed)
		}
	}
	return scoped, nil
}

func (s *scopedEnvelopeStore) GetEnvelopeBlobRefs(ctx context.Context, protocol, kind, key string) ([]string, error) {
	return s.base.GetEnvelopeBlobRefs(ctx, protocol, kind, s.scopedKey(key))
}

func (s *scopedEnvelopeStore) UpdateEnvelope(ctx context.Context, protocol, kind, key string, fn func(*metadb.MetadataEnvelope) (*metadb.MetadataEnvelope, error)) error {
	return s.base.UpdateEnvelope(ctx, protocol, kind, s.scopedKey(key), fn)
}

func newGitContentCacheUpstream(client *http.Client, logger *slog.Logger, credentials CredentialProvider) *ccgit.Upstream {
	upstreamOptions := []ccgit.UpstreamOption{
		ccgit.WithHTTPClient(client),
		ccgit.WithUpstreamLogger(logger),
	}
	if authProvider := newContentCacheGitBasicAuthProvider(credentials); authProvider != nil {
		upstreamOptions = append(upstreamOptions, ccgit.WithBasicAuthProvider(authProvider))
	}
	return ccgit.NewUpstream(upstreamOptions...)
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

var defaultOCIRegistryMappings = map[string]string{
	"docker.io":            "https://registry-1.docker.io",
	"index.docker.io":      "https://registry-1.docker.io",
	"registry-1.docker.io": "https://registry-1.docker.io",
	"ghcr.io":              "https://ghcr.io",
	"public.ecr.aws":       "https://public.ecr.aws",
}

var defaultOCIMirrorHosts = []string{
	"ghcr.io",
	"public.ecr.aws",
}

func normalizeOCIRegistryMappings(registries map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(defaultOCIRegistryMappings)+len(registries))
	for prefix, registryURL := range defaultOCIRegistryMappings {
		out[prefix] = registryURL
	}

	for prefix, registryURL := range registries {
		normalizedPrefix := strings.ToLower(strings.TrimSpace(prefix))
		normalizedURL := strings.TrimRight(strings.TrimSpace(registryURL), "/")
		if normalizedPrefix == "" || normalizedURL == "" {
			continue
		}
		if !isRegistryHostPrefix(normalizedPrefix) {
			return nil, fmt.Errorf("registry mapping key %q must be a registry host", prefix)
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

func ociMirrorHosts(registries map[string]string) []string {
	out := make([]string, 0, len(defaultOCIMirrorHosts)+len(registries))
	seen := make(map[string]struct{}, len(defaultOCIMirrorHosts)+len(registries))
	for _, prefix := range defaultOCIMirrorHosts {
		normalizedPrefix := strings.ToLower(strings.TrimSpace(prefix))
		if normalizedPrefix == "" {
			continue
		}
		seen[normalizedPrefix] = struct{}{}
		out = append(out, normalizedPrefix)
	}
	for prefix := range registries {
		normalizedPrefix := strings.ToLower(strings.TrimSpace(prefix))
		if normalizedPrefix == "" || strings.Contains(normalizedPrefix, "/") || !isRegistryHostPrefix(normalizedPrefix) || isDockerHubMirrorPrefix(normalizedPrefix) {
			continue
		}
		if _, ok := seen[normalizedPrefix]; ok {
			continue
		}
		seen[normalizedPrefix] = struct{}{}
		out = append(out, normalizedPrefix)
	}
	slices.Sort(out)
	return out
}

func isDockerHubMirrorPrefix(prefix string) bool {
	switch strings.ToLower(strings.TrimSpace(prefix)) {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return true
	default:
		return false
	}
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
