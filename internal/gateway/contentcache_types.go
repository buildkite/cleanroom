package gateway

import (
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
)

type gitHandlerFactory func(host string) (http.Handler, error)

type ociHandlerFactory func(prefix string) (ociHandlerEntry, error)

type ociRouteResolver func(prefix string) (ociRoute, error)

type ociHandlerEntry struct {
	handler      http.Handler
	policyHost   string
	policyPort   int
	upstreamHost string
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
}

// Close releases resources held by the content cache.
func (c *ContentCache) Close() error {
	if c == nil {
		return nil
	}

	c.ociMu.Lock()
	closers := make([]io.Closer, 0, len(c.ociHandlers)+1)
	for _, entry := range c.ociHandlers {
		if entry.closer != nil {
			closers = append(closers, entry.closer)
		}
	}
	c.ociMu.Unlock()

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

// OCIUpstreamForPrefix returns policy/upstream metadata for the requested
// registry prefix without allocating a new per-prefix handler.
func (c *ContentCache) OCIUpstreamForPrefix(prefix string) (string, int, string, error) {
	if c == nil || c.resolveOCIRoute == nil {
		return "", 0, "", errors.New("oci cache not configured")
	}

	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		return "", 0, "", errors.New("empty registry prefix")
	}

	c.ociMu.Lock()
	if entry, ok := c.ociHandlers[prefix]; ok {
		c.ociMu.Unlock()
		return entry.policyHost, entry.policyPort, entry.upstreamHost, nil
	}
	c.ociMu.Unlock()

	route, err := c.resolveOCIRoute(prefix)
	if err != nil {
		return "", 0, "", err
	}
	return route.policyHost, route.policyPort, route.upstreamHost, nil
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

func validateRedirectTargetPolicy(req *http.Request) error {
	scope, ok := ScopeFromContext(req.Context())
	if !ok || scope == nil || scope.Policy == nil {
		return errors.New("sandbox scope is required for redirect validation")
	}

	host, port, err := registryHostPort(req.URL.String())
	if err != nil {
		return err
	}
	if !scope.Policy.Allows(host, port) {
		return fmt.Errorf("redirect target %s:%d is not allowed by sandbox policy", host, port)
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

func newGitContentCacheHTTPClient(credentials CredentialProvider) *http.Client {
	client := newUpstreamContentCacheHTTPClient(credentials)
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

func newOCIContentCacheHTTPClient(credentials CredentialProvider) *http.Client {
	client := newUpstreamContentCacheHTTPClient(credentials)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return nil
		}
		return validateRedirectTargetPolicy(req)
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
