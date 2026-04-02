package gateway

import (
	"io"
	"net/http"
	"strings"
)

// ContentCache holds the shared storage and protocol handlers.
type ContentCache struct {
	closer      io.Closer
	gitHandler  http.Handler
	ociHandler  http.Handler
	prefixHosts map[string]string // OCI prefix → upstream hostname
}

// Close releases resources held by the content cache.
func (c *ContentCache) Close() error {
	if c.closer != nil {
		return c.closer.Close()
	}
	return nil
}

// GitHandler returns the content-cache git handler, or nil if git caching is
// not configured.
func (c *ContentCache) GitHandler() http.Handler { return c.gitHandler }

// OCIHandler returns the content-cache OCI handler, or nil if OCI caching is
// not configured.
func (c *ContentCache) OCIHandler() http.Handler { return c.ociHandler }

// PrefixHosts returns a mapping of OCI router prefixes to upstream registry
// hostnames, used for policy evaluation.
func (c *ContentCache) PrefixHosts() map[string]string { return c.prefixHosts }

// registryHostname extracts the hostname from a registry URL for policy checks.
func registryHostname(registryURL string) string {
	trimmed := strings.TrimSpace(registryURL)
	trimmed = strings.TrimPrefix(trimmed, "https://")
	trimmed = strings.TrimPrefix(trimmed, "http://")
	host, _, _ := strings.Cut(trimmed, "/")
	hostname, _, _ := strings.Cut(host, ":")
	return strings.ToLower(hostname)
}

// credentialInjector is an http.RoundTripper that resolves per-request
// credentials via a CredentialProvider before forwarding to the underlying
// transport.
type credentialInjector struct {
	base        http.RoundTripper
	credentials CredentialProvider
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
