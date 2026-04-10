package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

type stubRegistryPrefixHandlerProvider struct {
	handler      http.Handler
	policyHost   string
	upstreamHost string
	err          error
	prefix       string
}

func (s *stubRegistryPrefixHandlerProvider) OCIHandlerForPrefix(prefix string) (http.Handler, string, string, error) {
	s.prefix = prefix
	if s.err != nil {
		return nil, "", "", s.err
	}
	return s.handler, s.policyHost, s.upstreamHost, nil
}

func registryTestScope(allowedHosts ...string) *SandboxScope {
	allow := make([]policy.AllowRule, 0, len(allowedHosts))
	for _, host := range allowedHosts {
		allow = append(allow, policy.AllowRule{Host: host, Ports: []int{443}})
	}
	return &SandboxScope{
		SandboxID: "sandbox-registry-test",
		GuestIP:   "10.1.1.2",
		Policy: &policy.CompiledPolicy{
			Version:        1,
			NetworkDefault: "deny",
			Allow:          allow,
		},
	}
}

func TestCachedRegistryHandlerRewritesPathToV2(t *testing.T) {
	t.Parallel()

	var capturedPath string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	provider := &stubRegistryPrefixHandlerProvider{
		handler:      backend,
		policyHost:   "docker.io",
		upstreamHost: "registry-1.docker.io",
	}
	h := newCachedRegistryHandler(provider, nil)

	req := httptest.NewRequest("GET", "/registry/docker.io/library/nginx/manifests/latest", nil)
	req = withScope(req, registryTestScope("docker.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if want := "/v2/docker.io/library/nginx/manifests/latest"; capturedPath != want {
		t.Fatalf("expected backend path %q, got %q", want, capturedPath)
	}
	if provider.prefix != "docker.io" {
		t.Fatalf("expected registry prefix docker.io, got %q", provider.prefix)
	}
}

func TestCachedRegistryHandlerPolicyDeniesUnallowedHost(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called for denied host")
	})
	h := newCachedRegistryHandler(&stubRegistryPrefixHandlerProvider{
		handler:      backend,
		policyHost:   "docker.io",
		upstreamHost: "registry-1.docker.io",
	}, nil)

	req := httptest.NewRequest("GET", "/registry/docker.io/library/nginx/manifests/latest", nil)
	req = withScope(req, registryTestScope("ghcr.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if got := w.Header().Get(reasonCodeHeader); got != reasonHostNotAllowed {
		t.Fatalf("expected reason %s, got %q", reasonHostNotAllowed, got)
	}
}

func TestCachedRegistryHandlerUnknownPrefix(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called for unknown prefix")
	})
	h := newCachedRegistryHandler(&stubRegistryPrefixHandlerProvider{
		handler: backend,
		err:     errors.New("unknown prefix"),
	}, nil)

	req := httptest.NewRequest("GET", "/registry/unknown-prefix/library/nginx/manifests/latest", nil)
	req = withScope(req, registryTestScope("registry-1.docker.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestCachedRegistryHandlerNoScope(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called without scope")
	})
	h := newCachedRegistryHandler(&stubRegistryPrefixHandlerProvider{handler: backend}, nil)

	req := httptest.NewRequest("GET", "/registry/docker.io/library/nginx/manifests/latest", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestCachedRegistryHandlerRejectsPost(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called for POST")
	})
	h := newCachedRegistryHandler(&stubRegistryPrefixHandlerProvider{handler: backend}, nil)

	req := httptest.NewRequest("POST", "/registry/docker.io/library/nginx/manifests/latest", nil)
	req = withScope(req, registryTestScope("registry-1.docker.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestCachedRegistryHandlerBlobsRewritePath(t *testing.T) {
	t.Parallel()

	var capturedPath string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	provider := &stubRegistryPrefixHandlerProvider{
		handler:      backend,
		policyHost:   "ghcr.io",
		upstreamHost: "ghcr.io",
	}
	h := newCachedRegistryHandler(provider, nil)

	req := httptest.NewRequest("GET", "/registry/ghcr.io/myorg/myimage/blobs/sha256:abc123", nil)
	req = withScope(req, registryTestScope("ghcr.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if want := "/v2/ghcr.io/myorg/myimage/blobs/sha256:abc123"; capturedPath != want {
		t.Fatalf("expected backend path %q, got %q", want, capturedPath)
	}
	if provider.prefix != "ghcr.io" {
		t.Fatalf("expected registry prefix ghcr.io, got %q", provider.prefix)
	}
}

func TestCachedRegistryHandlerHeadAllowed(t *testing.T) {
	t.Parallel()

	var capturedMethod string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		w.WriteHeader(http.StatusOK)
	})
	h := newCachedRegistryHandler(&stubRegistryPrefixHandlerProvider{
		handler:      backend,
		policyHost:   "docker.io",
		upstreamHost: "registry-1.docker.io",
	}, nil)

	req := httptest.NewRequest("HEAD", "/registry/docker.io/library/nginx/manifests/latest", nil)
	req = withScope(req, registryTestScope("docker.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if capturedMethod != "HEAD" {
		t.Fatalf("expected HEAD, got %s", capturedMethod)
	}
}

func TestCachedRegistryHandlerEmptyPrefix(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called for empty prefix")
	})
	h := newCachedRegistryHandler(&stubRegistryPrefixHandlerProvider{handler: backend}, nil)

	req := httptest.NewRequest("GET", "/registry/", nil)
	req = withScope(req, registryTestScope("registry-1.docker.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestRegistryHostname(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"https://registry-1.docker.io", "registry-1.docker.io"},
		{"https://ghcr.io/v2/", "ghcr.io"},
		{"http://localhost:5000", "localhost"},
		{"registry-1.docker.io", "registry-1.docker.io"},
		{"https://GHCR.IO", "ghcr.io"},
	}
	for _, tt := range tests {
		got := registryHostname(tt.input)
		if got != tt.want {
			t.Errorf("registryHostname(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestResolveOCIRegistryRouteUsesDockerHubAlias(t *testing.T) {
	t.Parallel()

	routes, err := normalizeOCIRegistryMappings(nil)
	if err != nil {
		t.Fatalf("normalizeOCIRegistryMappings returned error: %v", err)
	}

	route, err := resolveOCIRegistryRoute("docker.io", routes)
	if err != nil {
		t.Fatalf("resolveOCIRegistryRoute returned error: %v", err)
	}
	if route.policyHost != "docker.io" {
		t.Fatalf("expected docker.io policy host, got %q", route.policyHost)
	}
	if route.upstreamHost != "registry-1.docker.io" {
		t.Fatalf("expected Docker Hub upstream host, got %q", route.upstreamHost)
	}
	if route.upstreamURL != "https://registry-1.docker.io" {
		t.Fatalf("expected Docker Hub upstream URL, got %q", route.upstreamURL)
	}
}
