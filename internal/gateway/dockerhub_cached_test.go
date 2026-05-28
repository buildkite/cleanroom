package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDockerHubMirrorHandlerRewritesManifestPath(t *testing.T) {
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
	h := newDockerHubMirrorHandler(provider, nil, false)

	req := httptest.NewRequest(http.MethodGet, "/v2/library/alpine/manifests/latest", nil)
	req = withScope(req, registryTestScope("docker.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if want := "/v2/docker.io/library/alpine/manifests/latest"; capturedPath != want {
		t.Fatalf("expected backend path %q, got %q", want, capturedPath)
	}
	if got := provider.prefix; got != dockerHubMirrorPrefix {
		t.Fatalf("expected prefix %q, got %q", dockerHubMirrorPrefix, got)
	}
}

func TestDockerHubMirrorHandlerRewritesVersionProbe(t *testing.T) {
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
	h := newDockerHubMirrorHandler(provider, nil, false)

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req = withScope(req, registryTestScope("docker.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if want := "/v2/"; capturedPath != want {
		t.Fatalf("expected backend path %q, got %q", want, capturedPath)
	}
}

func TestDockerHubMirrorHandlerDeniesPolicyMiss(t *testing.T) {
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
	h := newDockerHubMirrorHandler(provider, nil, false)

	req := httptest.NewRequest(http.MethodGet, "/v2/library/alpine/manifests/latest", nil)
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

func TestDockerHubMirrorHandlerDeniesRepoOutsideGatewayEnvelope(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called for unauthorized repo")
	})
	provider := &stubRegistryPrefixHandlerProvider{
		handler:      backend,
		policyHost:   "docker.io",
		policyPort:   443,
		upstreamHost: "registry-1.docker.io",
		upstreamPort: 443,
	}
	h := newDockerHubMirrorHandler(provider, nil, true)

	req := httptest.NewRequest(http.MethodGet, "/v2/library/redis/manifests/latest", nil)
	req = withScope(req, registryOwnedScope([]string{"docker.io/library/alpine"}, "docker.io"))
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

func TestDockerHubMirrorHandlerAllowsRepoInGatewayEnvelope(t *testing.T) {
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
	h := newDockerHubMirrorHandler(provider, nil, true)

	req := httptest.NewRequest(http.MethodGet, "/v2/library/alpine/manifests/latest", nil)
	req = withScope(req, registryOwnedScope([]string{"docker.io/library/alpine"}, "docker.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if capturedPath == "" {
		t.Fatal("expected cache handler to be called")
	}
}

func TestDockerHubMirrorHandlerRejectsPost(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called for POST")
	})
	h := newDockerHubMirrorHandler(&stubRegistryPrefixHandlerProvider{handler: backend}, nil, false)

	req := httptest.NewRequest(http.MethodPost, "/v2/library/alpine/manifests/latest", nil)
	req = withScope(req, registryTestScope("docker.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestDockerHubMirrorHandlerRequiresConfiguredOCI(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called when docker hub mirror is unavailable")
	})
	h := newDockerHubMirrorHandler(&stubRegistryPrefixHandlerProvider{
		handler: backend,
		err:     errors.New("unknown prefix"),
	}, nil, false)

	req := httptest.NewRequest(http.MethodGet, "/v2/library/alpine/manifests/latest", nil)
	req = withScope(req, registryTestScope("docker.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
