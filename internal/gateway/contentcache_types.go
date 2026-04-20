package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/policy"
)

const defaultMaxGoProxyScopedHandlers = 32

type gitHandlerFactory func(host string) (http.Handler, error)

type ociHandlerFactory func(prefix string) (ociHandlerEntry, error)

type ociRouteResolver func(prefix string) (ociRoute, error)

type ociHandlerEntry struct {
	handler      http.Handler
	policyHost   string
	policyPort   int
	upstreamHost string
	upstreamPort int
	closer       io.Closer
}

type goProxyScopedHandler struct {
	handler http.Handler
	closer  io.Closer
}

type goProxyHandlerEntry struct {
	policyHost   string
	policyPort   int
	upstreamHost string
	upstreamPort int
	mu           sync.Mutex
	handlers     map[string]goProxyScopedHandler
	order        []string
	maxHandlers  int
	buildHandler func(*policy.CompiledPolicy) (goProxyScopedHandler, error)
}

type sumDBHandlerEntry struct {
	handler      http.Handler
	policyHost   string
	policyPort   int
	upstreamHost string
	upstreamPort int
	name         string
	closer       io.Closer
}

type rubyGemsHandlerEntry struct {
	handler      http.Handler
	policyHost   string
	policyPort   int
	upstreamHost string
	upstreamPort int
	closer       io.Closer
}

type fetchHandlerEntry struct {
	handler      http.Handler
	allowedHosts map[string]struct{}
	closer       io.Closer
}

// ContentCache holds the shared storage and protocol handlers.
type ContentCache struct {
	closer io.Closer

	gitMu           sync.Mutex
	gitHandlers     map[string]http.Handler
	buildGitHandler gitHandlerFactory

	ociMu           sync.Mutex
	ociHandlers     map[string]ociHandlerEntry
	buildOCIHandler ociHandlerFactory
	resolveOCIRoute ociRouteResolver

	goProxy  goProxyHandlerEntry
	sumdb    sumDBHandlerEntry
	rubyGems rubyGemsHandlerEntry
	fetch    fetchHandlerEntry
}

// Close releases resources held by the content cache.
func (c *ContentCache) Close() error {
	if c == nil {
		return nil
	}

	c.ociMu.Lock()
	closers := make([]io.Closer, 0, len(c.ociHandlers)+4)
	for _, entry := range c.ociHandlers {
		if entry.closer != nil {
			closers = append(closers, entry.closer)
		}
	}
	c.ociMu.Unlock()
	c.goProxy.mu.Lock()
	for _, entry := range c.goProxy.handlers {
		if entry.closer != nil {
			closers = append(closers, entry.closer)
		}
	}
	c.goProxy.mu.Unlock()
	if c.sumdb.closer != nil {
		closers = append(closers, c.sumdb.closer)
	}
	if c.rubyGems.closer != nil {
		closers = append(closers, c.rubyGems.closer)
	}
	if c.fetch.closer != nil {
		closers = append(closers, c.fetch.closer)
	}

	if c.closer != nil {
		closers = append(closers, c.closer)
	}

	errs := make([]error, 0, len(closers))
	for _, closer := range closers {
		if err := closer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// HasGitHandler reports whether git caching is configured.
func (c *ContentCache) HasGitHandler() bool {
	return c != nil && c.buildGitHandler != nil
}

// GitHandlerForHost returns a git cache handler scoped to the requested host.
func (c *ContentCache) GitHandlerForHost(host string) (http.Handler, error) {
	if c == nil || c.buildGitHandler == nil {
		return nil, errors.New("git cache not configured")
	}

	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return nil, errors.New("empty git host")
	}

	c.gitMu.Lock()
	defer c.gitMu.Unlock()

	if handler, ok := c.gitHandlers[host]; ok {
		return handler, nil
	}

	handler, err := c.buildGitHandler(host)
	if err != nil {
		return nil, err
	}
	c.gitHandlers[host] = handler
	return handler, nil
}

// HasOCIHandler reports whether OCI caching is configured.
func (c *ContentCache) HasOCIHandler() bool {
	return c != nil && c.buildOCIHandler != nil
}

// HasGoProxyHandler reports whether Go module proxy caching is configured.
func (c *ContentCache) HasGoProxyHandler() bool {
	return c != nil && c.goProxy.buildHandler != nil
}

// GoProxyUpstream returns policy/upstream metadata for the configured Go module proxy.
func (c *ContentCache) GoProxyUpstream() (string, int, string, int, error) {
	if c == nil || c.goProxy.buildHandler == nil {
		return "", 0, "", 0, errors.New("goproxy cache not configured")
	}
	return c.goProxy.policyHost, c.goProxy.policyPort, c.goProxy.upstreamHost, c.goProxy.upstreamPort, nil
}

// GoProxyHandlerForPolicy returns a Go module proxy cache handler scoped to
// the provided compiled policy. The handler caches background fills against the
// same allowlist that authorized the originating sandbox request.
func (c *ContentCache) GoProxyHandlerForPolicy(compiled *policy.CompiledPolicy) (http.Handler, error) {
	if c == nil || c.goProxy.buildHandler == nil {
		return nil, errors.New("goproxy cache not configured")
	}
	if compiled == nil {
		return nil, errors.New("goproxy policy is required")
	}

	key := strings.TrimSpace(compiled.Hash)
	if key == "" {
		key = fmt.Sprintf("%p", compiled)
	}

	c.goProxy.mu.Lock()

	if entry, ok := c.goProxy.handlers[key]; ok {
		c.goProxy.touchLocked(key)
		c.goProxy.mu.Unlock()
		return entry.handler, nil
	}

	entry, err := c.goProxy.buildHandler(compiled)
	if err != nil {
		c.goProxy.mu.Unlock()
		return nil, err
	}
	if c.goProxy.handlers == nil {
		c.goProxy.handlers = make(map[string]goProxyScopedHandler)
	}
	c.goProxy.handlers[key] = entry
	c.goProxy.touchLocked(key)
	evicted := c.goProxy.evictLocked()
	c.goProxy.mu.Unlock()
	if evicted != nil {
		_ = evicted.Close()
	}
	return entry.handler, nil
}

func (e *goProxyHandlerEntry) touchLocked(key string) {
	for i, existing := range e.order {
		if existing != key {
			continue
		}
		copy(e.order[i:], e.order[i+1:])
		e.order = e.order[:len(e.order)-1]
		break
	}
	e.order = append(e.order, key)
}

func (e *goProxyHandlerEntry) evictLocked() io.Closer {
	maxHandlers := e.maxHandlers
	if maxHandlers <= 0 {
		maxHandlers = defaultMaxGoProxyScopedHandlers
	}
	if len(e.order) <= maxHandlers {
		return nil
	}

	evictedKey := e.order[0]
	e.order = e.order[1:]
	entry, ok := e.handlers[evictedKey]
	if !ok {
		return nil
	}
	delete(e.handlers, evictedKey)
	return entry.closer
}

// HasSumDBHandler reports whether sumdb caching is configured.
func (c *ContentCache) HasSumDBHandler() bool {
	return c != nil && c.sumdb.handler != nil
}

// SumDBUpstream returns policy/upstream metadata for the configured checksum database proxy.
func (c *ContentCache) SumDBUpstream() (string, int, string, int, error) {
	if c == nil || c.sumdb.handler == nil {
		return "", 0, "", 0, errors.New("sumdb cache not configured")
	}
	return c.sumdb.policyHost, c.sumdb.policyPort, c.sumdb.upstreamHost, c.sumdb.upstreamPort, nil
}

// SumDBHandler returns the configured checksum database proxy handler.
func (c *ContentCache) SumDBHandler() (http.Handler, error) {
	if c == nil || c.sumdb.handler == nil {
		return nil, errors.New("sumdb cache not configured")
	}
	return c.sumdb.handler, nil
}

// HasRubyGemsHandler reports whether RubyGems caching is configured.
func (c *ContentCache) HasRubyGemsHandler() bool {
	return c != nil && c.rubyGems.handler != nil
}

// RubyGemsUpstream returns policy/upstream metadata for the configured
// RubyGems upstream.
func (c *ContentCache) RubyGemsUpstream() (string, int, string, int, error) {
	if c == nil || c.rubyGems.handler == nil {
		return "", 0, "", 0, errors.New("rubygems cache not configured")
	}
	return c.rubyGems.policyHost, c.rubyGems.policyPort, c.rubyGems.upstreamHost, c.rubyGems.upstreamPort, nil
}

// RubyGemsHandler returns the configured RubyGems cache handler.
func (c *ContentCache) RubyGemsHandler() (http.Handler, error) {
	if c == nil || c.rubyGems.handler == nil {
		return nil, errors.New("rubygems cache not configured")
	}
	return c.rubyGems.handler, nil
}

// HasFetchHandler reports whether immutable artifact fetch caching is configured.
func (c *ContentCache) HasFetchHandler() bool {
	return c != nil && c.fetch.handler != nil
}

// FetchAllowsHost reports whether the immutable artifact fetch route is configured for host.
func (c *ContentCache) FetchAllowsHost(host string) bool {
	if c == nil || c.fetch.handler == nil {
		return false
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	_, ok := c.fetch.allowedHosts[host]
	return ok
}

// FetchHandler returns the configured immutable artifact fetch cache handler.
func (c *ContentCache) FetchHandler() (http.Handler, error) {
	if c == nil || c.fetch.handler == nil {
		return nil, errors.New("fetch cache not configured")
	}
	return c.fetch.handler, nil
}

// OCIUpstreamForPrefix returns policy/upstream metadata for the requested
// registry prefix without allocating a new per-prefix handler.
func (c *ContentCache) OCIUpstreamForPrefix(prefix string) (string, int, string, int, error) {
	if c == nil || c.resolveOCIRoute == nil {
		return "", 0, "", 0, errors.New("oci cache not configured")
	}

	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		return "", 0, "", 0, errors.New("empty registry prefix")
	}

	c.ociMu.Lock()
	if entry, ok := c.ociHandlers[prefix]; ok {
		c.ociMu.Unlock()
		return entry.policyHost, entry.policyPort, entry.upstreamHost, entry.upstreamPort, nil
	}
	c.ociMu.Unlock()

	route, err := c.resolveOCIRoute(prefix)
	if err != nil {
		return "", 0, "", 0, err
	}
	return route.policyHost, route.policyPort, route.upstreamHost, route.upstreamPort, nil
}

// OCIHandlerForPrefix returns an OCI cache handler for the requested registry
// prefix, creating and caching it on first use.
func (c *ContentCache) OCIHandlerForPrefix(prefix string) (http.Handler, error) {
	if c == nil || c.buildOCIHandler == nil {
		return nil, errors.New("oci cache not configured")
	}

	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		return nil, errors.New("empty registry prefix")
	}

	c.ociMu.Lock()
	defer c.ociMu.Unlock()

	if entry, ok := c.ociHandlers[prefix]; ok {
		return entry.handler, nil
	}

	entry, err := c.buildOCIHandler(prefix)
	if err != nil {
		return nil, err
	}
	c.ociHandlers[prefix] = entry
	return entry.handler, nil
}

// registryHostname extracts the hostname from a registry URL for policy checks.
func registryHostname(registryURL string) string {
	host, _, err := registryHostPort(registryURL)
	if err != nil {
		return ""
	}
	return host
}

func registryHostPort(registryURL string) (string, int, error) {
	trimmed := strings.TrimSpace(registryURL)
	if trimmed == "" {
		return "", 0, errors.New("empty registry URL")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", 0, fmt.Errorf("parse registry URL: %w", err)
	}
	if parsed.Host == "" {
		return "", 0, errors.New("missing registry host")
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", 0, errors.New("missing registry host")
	}

	if portStr := parsed.Port(); portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 || port > 65535 {
			return "", 0, fmt.Errorf("invalid registry port %q", portStr)
		}
		return host, port, nil
	}

	if strings.EqualFold(parsed.Scheme, "http") {
		return host, 80, nil
	}
	return host, 443, nil
}

type ociUpstreamPolicyContext struct {
	policyHost   string
	policyPort   int
	upstreamHost string
	upstreamPort int
}

type ociUpstreamPolicyContextKey struct{}

func withOCIUpstreamPolicy(ctx context.Context, policyHost string, policyPort int, upstreamHost string, upstreamPort int) context.Context {
	return context.WithValue(ctx, ociUpstreamPolicyContextKey{}, ociUpstreamPolicyContext{
		policyHost:   strings.ToLower(strings.TrimSpace(policyHost)),
		policyPort:   policyPort,
		upstreamHost: strings.ToLower(strings.TrimSpace(upstreamHost)),
		upstreamPort: upstreamPort,
	})
}

func ociUpstreamPolicyFromContext(ctx context.Context) (ociUpstreamPolicyContext, bool) {
	policy, ok := ctx.Value(ociUpstreamPolicyContextKey{}).(ociUpstreamPolicyContext)
	return policy, ok
}

func validateUpstreamTargetPolicy(req *http.Request, compiled *policy.CompiledPolicy) error {
	if scope, ok := ScopeFromContext(req.Context()); ok && scope != nil && scope.Policy != nil {
		compiled = scope.Policy
	}
	if compiled == nil {
		return errors.New("sandbox scope is required for upstream policy validation")
	}

	host, port, err := registryHostPort(req.URL.String())
	if err != nil {
		return err
	}
	policyHost := host
	policyPort := port
	if mapped, ok := ociUpstreamPolicyFromContext(req.Context()); ok && host == mapped.upstreamHost && port == mapped.upstreamPort {
		policyHost = mapped.policyHost
		policyPort = mapped.policyPort
	}
	if !compiled.Allows(policyHost, policyPort) {
		return fmt.Errorf("upstream target %s:%d is not allowed by sandbox policy", policyHost, policyPort)
	}
	return nil
}

// credentialInjector is an http.RoundTripper that resolves per-request
// credentials via a CredentialProvider before forwarding to the underlying
// transport.
type credentialInjector struct {
	base        http.RoundTripper
	credentials CredentialProvider
}

type policyValidatingRoundTripper struct {
	base   http.RoundTripper
	policy *policy.CompiledPolicy
}

func newGitContentCacheHTTPClient(credentials CredentialProvider) *http.Client {
	client := newUpstreamContentCacheHTTPClient(credentials)
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

func newOCIContentCacheHTTPClient(credentials CredentialProvider) *http.Client {
	return newPolicyValidatingContentCacheHTTPClient(credentials, nil)
}

func newGoProxyContentCacheHTTPClient(compiled *policy.CompiledPolicy) *http.Client {
	return newPolicyValidatingContentCacheHTTPClient(nil, compiled)
}

func newSumDBContentCacheHTTPClient() *http.Client {
	return newPolicyValidatingContentCacheHTTPClient(nil, nil)
}

func newRubyGemsContentCacheHTTPClient(_ CredentialProvider) *http.Client {
	return newPolicyValidatingContentCacheHTTPClient(nil, nil)
}

func newFetchContentCacheHTTPClient() *http.Client {
	return newPolicyValidatingContentCacheHTTPClient(nil, nil)
}

func newPolicyValidatingContentCacheHTTPClient(credentials CredentialProvider, compiled *policy.CompiledPolicy) *http.Client {
	client := newUpstreamContentCacheHTTPClient(credentials)
	client.Transport = &policyValidatingRoundTripper{base: client.Transport, policy: compiled}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return nil
		}
		return validateUpstreamTargetPolicy(req, compiled)
	}
	return client
}

func newUpstreamContentCacheHTTPClient(credentials CredentialProvider) *http.Client {
	return &http.Client{
		Transport: &credentialInjector{
			base: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: defaultUpstreamTimeout}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: defaultUpstreamTimeout,
				// Disable keep-alives to avoid sharing any upstream connection pool
				// across sandbox identities.
				DisableKeepAlives: true,
			},
			credentials: credentials,
		},
	}
}

func (c *credentialInjector) RoundTrip(r *http.Request) (*http.Response, error) {
	if c.credentials != nil && r.Header.Get("Authorization") == "" {
		remoteURL := canonicalRemoteFromRequest(r)
		header, err := c.credentials.Resolve(r.Context(), remoteURL)
		if err == nil && header != "" {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", header)
		}
	}
	return c.base.RoundTrip(r)
}

func (p *policyValidatingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := validateUpstreamTargetPolicy(r, p.policy); err != nil {
		return nil, err
	}
	return p.base.RoundTrip(r)
}

// canonicalRemoteFromRequest strips Git Smart HTTP path suffixes to recover
// the bare repository URL that CredentialProvider.Resolve expects.
func canonicalRemoteFromRequest(r *http.Request) string {
	path := r.URL.Path
	for _, suffix := range []string{"/info/refs", "/git-upload-pack", "/git-receive-pack"} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
			break
		}
	}
	return r.URL.Scheme + "://" + r.URL.Host + path
}

func isRegistryHostPrefix(component string) bool {
	component = strings.TrimSpace(strings.ToLower(component))
	if component == "" {
		return false
	}
	if component == "localhost" {
		return true
	}
	return strings.Contains(component, ".") || strings.Contains(component, ":")
}

type closeFunc func()

func (f closeFunc) Close() error {
	f()
	return nil
}
