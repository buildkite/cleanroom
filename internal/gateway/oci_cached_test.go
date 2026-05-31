package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/buildkite/cleanroom/internal/gatewayauth"
	"github.com/buildkite/cleanroom/internal/policy"
)

type stubRegistryPrefixHandlerProvider struct {
	handler      http.Handler
	policyHost   string
	policyPort   int
	upstreamHost string
	upstreamPort int
	err          error
	prefix       string
	cacheScope   string
	handlerCalls int
	lookupCalls  int
}

func (s *stubRegistryPrefixHandlerProvider) OCIUpstreamForPrefix(prefix string) (string, int, string, int, error) {
	s.prefix = prefix
	s.lookupCalls++
	if s.err != nil {
		return "", 0, "", 0, s.err
	}
	return s.policyHost, s.policyPort, s.upstreamHost, s.upstreamPort, nil
}

func (s *stubRegistryPrefixHandlerProvider) OCIHandlerForPrefix(prefix, cacheScope string) (http.Handler, func(), error) {
	s.prefix = prefix
	s.cacheScope = cacheScope
	s.handlerCalls++
	if s.err != nil {
		return nil, nil, s.err
	}
	return s.handler, func() {}, nil
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

func registryOwnedScope(ociPrefixes []string, allowedHosts ...string) *SandboxScope {
	scope := registryTestScope(allowedHosts...)
	scope.GatewayScope = gatewayauth.ScopeMetadata{
		Owner: gatewayauth.Owner{
			PrincipalID: "oidc:test:alice",
			Scope:       "repo:buildkite/cleanroom",
		},
		Authorization: gatewayauth.Authorization{
			OCIRepoPrefixes: ociPrefixes,
		},
	}
	return scope
}

func registryTestScopeWithRule(host string, ports ...int) *SandboxScope {
	return &SandboxScope{
		SandboxID: "sandbox-registry-test",
		GuestIP:   "10.1.1.2",
		Policy: &policy.CompiledPolicy{
			Version:        1,
			NetworkDefault: "deny",
			Allow: []policy.AllowRule{
				{Host: host, Ports: ports},
			},
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
		policyPort:   443,
		upstreamHost: "registry-1.docker.io",
		upstreamPort: 443,
	}
	h := newCachedRegistryHandler(provider, nil, false)

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
	provider := &stubRegistryPrefixHandlerProvider{
		handler:      backend,
		policyHost:   "docker.io",
		policyPort:   443,
		upstreamHost: "registry-1.docker.io",
		upstreamPort: 443,
	}
	h := newCachedRegistryHandler(provider, nil, false)

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
	if provider.lookupCalls != 1 {
		t.Fatalf("expected one metadata lookup, got %d", provider.lookupCalls)
	}
	if provider.handlerCalls != 0 {
		t.Fatalf("expected denied request to avoid handler creation, got %d handler calls", provider.handlerCalls)
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
	}, nil, false)

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
	h := newCachedRegistryHandler(&stubRegistryPrefixHandlerProvider{handler: backend}, nil, false)

	req := httptest.NewRequest("GET", "/registry/docker.io/library/nginx/manifests/latest", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestCachedRegistryHandlerRequiresOwnerWhenConfigured(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called without owner")
	})
	provider := &stubRegistryPrefixHandlerProvider{
		handler:      backend,
		policyHost:   "docker.io",
		policyPort:   443,
		upstreamHost: "registry-1.docker.io",
		upstreamPort: 443,
	}
	h := newCachedRegistryHandler(provider, nil, true)

	req := httptest.NewRequest("GET", "/registry/docker.io/library/nginx/manifests/latest", nil)
	req = withScope(req, registryTestScope("docker.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if got := w.Header().Get(reasonCodeHeader); got != reasonGatewayAuthDenied {
		t.Fatalf("expected reason %s, got %q", reasonGatewayAuthDenied, got)
	}
	if provider.handlerCalls != 0 {
		t.Fatalf("expected denied request to avoid handler creation, got %d handler calls", provider.handlerCalls)
	}
}

func TestCachedRegistryHandlerDeniesRepoOutsideGatewayEnvelope(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called for unauthorized repo")
	})
	provider := &stubRegistryPrefixHandlerProvider{
		handler:      backend,
		policyHost:   "ghcr.io",
		policyPort:   443,
		upstreamHost: "ghcr.io",
		upstreamPort: 443,
	}
	h := newCachedRegistryHandler(provider, nil, true)

	req := httptest.NewRequest("GET", "/registry/ghcr.io/buildkite/private/manifests/latest", nil)
	req = withScope(req, registryOwnedScope([]string{"ghcr.io/buildkite/cleanroom-base/alpine"}, "ghcr.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if provider.handlerCalls != 0 {
		t.Fatalf("expected denied request to avoid handler creation, got %d handler calls", provider.handlerCalls)
	}
}

func TestCachedRegistryHandlerAllowsRepoInGatewayEnvelope(t *testing.T) {
	t.Parallel()

	var capturedPath string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	provider := &stubRegistryPrefixHandlerProvider{
		handler:      backend,
		policyHost:   "ghcr.io",
		policyPort:   443,
		upstreamHost: "ghcr.io",
		upstreamPort: 443,
	}
	h := newCachedRegistryHandler(provider, nil, true)

	req := httptest.NewRequest("GET", "/registry/ghcr.io/buildkite/cleanroom-base/alpine/manifests/latest", nil)
	req = withScope(req, registryOwnedScope([]string{"ghcr.io/buildkite/cleanroom-base/alpine"}, "ghcr.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if capturedPath == "" {
		t.Fatal("expected cache handler to be called")
	}
}

func TestCachedRegistryHandlerScopesCacheToAuthorizedRepoAndOwner(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	provider := &stubRegistryPrefixHandlerProvider{
		handler:      backend,
		policyHost:   "ghcr.io",
		policyPort:   443,
		upstreamHost: "ghcr.io",
		upstreamPort: 443,
	}
	h := newCachedRegistryHandler(provider, nil, true)

	req := httptest.NewRequest("GET", "/registry/ghcr.io/buildkite/cleanroom-base/alpine/blobs/sha256:abc123", nil)
	req = withScope(req, registryOwnedScope([]string{"ghcr.io/buildkite/cleanroom-base/alpine"}, "ghcr.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	want := "owner:oidc:test:alice\x00scope:repo:buildkite/cleanroom\x00oci:ghcr.io/buildkite/cleanroom-base/alpine"
	if provider.cacheScope != want {
		t.Fatalf("unexpected cache scope: got %q want %q", provider.cacheScope, want)
	}
}

func TestCachedRegistryHandlerRejectsPost(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called for POST")
	})
	h := newCachedRegistryHandler(&stubRegistryPrefixHandlerProvider{handler: backend}, nil, false)

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
		policyPort:   443,
		upstreamHost: "ghcr.io",
		upstreamPort: 443,
	}
	h := newCachedRegistryHandler(provider, nil, false)

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

func TestCachedRegistryHandlerRegistryProbeRewritesToVersionCheck(t *testing.T) {
	t.Parallel()

	var capturedPath string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	provider := &stubRegistryPrefixHandlerProvider{
		handler:      backend,
		policyHost:   "docker.io",
		policyPort:   443,
		upstreamHost: "registry-1.docker.io",
		upstreamPort: 443,
	}
	h := newCachedRegistryHandler(provider, nil, false)

	req := httptest.NewRequest("GET", "/registry/docker.io/v2/", nil)
	req = withScope(req, registryTestScope("docker.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if want := "/v2/"; capturedPath != want {
		t.Fatalf("expected backend path %q, got %q", want, capturedPath)
	}
	if provider.prefix != "docker.io" {
		t.Fatalf("expected registry prefix docker.io, got %q", provider.prefix)
	}
}

func TestCachedRegistryHandlerStripsClientV2Prefix(t *testing.T) {
	t.Parallel()

	var capturedPath string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	provider := &stubRegistryPrefixHandlerProvider{
		handler:      backend,
		policyHost:   "docker.io",
		policyPort:   443,
		upstreamHost: "registry-1.docker.io",
		upstreamPort: 443,
	}
	h := newCachedRegistryHandler(provider, nil, false)

	req := httptest.NewRequest("GET", "/registry/docker.io/v2/library/nginx/manifests/latest", nil)
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
		policyPort:   443,
		upstreamHost: "registry-1.docker.io",
		upstreamPort: 443,
	}, nil, false)

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
	provider := &stubRegistryPrefixHandlerProvider{handler: backend}
	h := newCachedRegistryHandler(provider, nil, false)

	req := httptest.NewRequest("GET", "/registry/", nil)
	req = withScope(req, registryTestScope("registry-1.docker.io"))
	req, obs := withGatewayRequestObservability(req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if got := w.Header().Get(reasonCodeHeader); got != reasonInvalidRequest {
		t.Fatalf("expected reason %s, got %q", reasonInvalidRequest, got)
	}
	requireGatewayRequestDecision(t, obs, gatewayActionDeny, reasonInvalidRequest)
	if provider.lookupCalls != 0 {
		t.Fatalf("expected invalid request to avoid metadata lookup, got %d lookups", provider.lookupCalls)
	}
}

func TestCachedRegistryHandlerBarePrefixSetsGatewayDecision(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called for bare prefix")
	})
	provider := &stubRegistryPrefixHandlerProvider{handler: backend}
	h := newCachedRegistryHandler(provider, nil, false)

	req := httptest.NewRequest("GET", "/registry/docker.io", nil)
	req = withScope(req, registryTestScope("docker.io"))
	req, obs := withGatewayRequestObservability(req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if got := w.Header().Get(reasonCodeHeader); got != reasonInvalidRequest {
		t.Fatalf("expected reason %s, got %q", reasonInvalidRequest, got)
	}
	requireGatewayRequestDecision(t, obs, gatewayActionDeny, reasonInvalidRequest)
	if provider.lookupCalls != 0 {
		t.Fatalf("expected invalid request to avoid metadata lookup, got %d lookups", provider.lookupCalls)
	}
}

func TestCachedRegistryHandlerUsesResolvedPolicyPort(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := newCachedRegistryHandler(&stubRegistryPrefixHandlerProvider{
		handler:      backend,
		policyHost:   "registry.internal",
		policyPort:   5000,
		upstreamHost: "registry.internal",
		upstreamPort: 5000,
	}, nil, false)

	req := httptest.NewRequest("GET", "/registry/internal/library/nginx/manifests/latest", nil)
	req = withScope(req, registryTestScopeWithRule("registry.internal", 5000))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
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
		{"https://[2001:db8::1]:5000/v2/", "2001:db8::1"},
	}
	for _, tt := range tests {
		got := registryHostname(tt.input)
		if got != tt.want {
			t.Errorf("registryHostname(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestRegistryHostPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		wantHost string
		wantPort int
	}{
		{"https://registry-1.docker.io", "registry-1.docker.io", 443},
		{"http://localhost:5000", "localhost", 5000},
		{"https://[2001:db8::1]:5000/v2/", "2001:db8::1", 5000},
		{"docker.io", "docker.io", 443},
	}

	for _, tt := range tests {
		gotHost, gotPort, err := registryHostPort(tt.input)
		if err != nil {
			t.Fatalf("registryHostPort(%q) returned error: %v", tt.input, err)
		}
		if gotHost != tt.wantHost || gotPort != tt.wantPort {
			t.Fatalf("registryHostPort(%q) = (%q, %d), want (%q, %d)", tt.input, gotHost, gotPort, tt.wantHost, tt.wantPort)
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
	if route.policyPort != 443 {
		t.Fatalf("expected docker.io policy port 443, got %d", route.policyPort)
	}
	if route.upstreamHost != "registry-1.docker.io" {
		t.Fatalf("expected Docker Hub upstream host, got %q", route.upstreamHost)
	}
	if route.upstreamURL != "https://registry-1.docker.io" {
		t.Fatalf("expected Docker Hub upstream URL, got %q", route.upstreamURL)
	}
}

func TestNormalizeOCIRegistryMappingsIncludesBuiltInPublicRegistries(t *testing.T) {
	t.Parallel()

	routes, err := normalizeOCIRegistryMappings(nil)
	if err != nil {
		t.Fatalf("normalizeOCIRegistryMappings returned error: %v", err)
	}
	if got, want := routes["ghcr.io"], "https://ghcr.io"; got != want {
		t.Fatalf("ghcr.io mapping = %q, want %q", got, want)
	}
	if got, want := routes["public.ecr.aws"], "https://public.ecr.aws"; got != want {
		t.Fatalf("public.ecr.aws mapping = %q, want %q", got, want)
	}
}

func TestNormalizeOCIRegistryMappingsRejectsSymbolicPrefix(t *testing.T) {
	t.Parallel()

	_, err := normalizeOCIRegistryMappings(map[string]string{
		"registry-cache": "https://registry.internal:5000",
	})
	if err == nil {
		t.Fatal("expected symbolic registry prefix to be rejected")
	}
	if got, want := err.Error(), `registry mapping key "registry-cache" must be a registry host`; got != want {
		t.Fatalf("unexpected error: got %q want %q", got, want)
	}
}

func TestResolveOCIRegistryRouteUsesHostStylePrefixPort(t *testing.T) {
	t.Parallel()

	routes, err := normalizeOCIRegistryMappings(map[string]string{
		"registry.internal:5000": "https://registry.internal",
	})
	if err != nil {
		t.Fatalf("normalizeOCIRegistryMappings returned error: %v", err)
	}

	route, err := resolveOCIRegistryRoute("registry.internal:5000", routes)
	if err != nil {
		t.Fatalf("resolveOCIRegistryRoute returned error: %v", err)
	}
	if route.policyHost != "registry.internal" {
		t.Fatalf("expected registry.internal policy host, got %q", route.policyHost)
	}
	if route.policyPort != 5000 {
		t.Fatalf("expected registry.internal policy port 5000, got %d", route.policyPort)
	}
	if route.upstreamHost != "registry.internal" {
		t.Fatalf("expected registry.internal upstream host, got %q", route.upstreamHost)
	}
	if route.upstreamURL != "https://registry.internal" {
		t.Fatalf("expected upstream URL https://registry.internal, got %q", route.upstreamURL)
	}
}
