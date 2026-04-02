package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

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
	h := newCachedRegistryHandler(backend, map[string]string{
		"docker-hub": "registry-1.docker.io",
	}, nil)

	req := httptest.NewRequest("GET", "/registry/docker-hub/library/nginx/manifests/latest", nil)
	req = withScope(req, registryTestScope("registry-1.docker.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if want := "/v2/docker-hub/library/nginx/manifests/latest"; capturedPath != want {
		t.Fatalf("expected backend path %q, got %q", want, capturedPath)
	}
}

func TestCachedRegistryHandlerPolicyDeniesUnallowedHost(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called for denied host")
	})
	h := newCachedRegistryHandler(backend, map[string]string{
		"docker-hub": "registry-1.docker.io",
	}, nil)

	// Scope only allows ghcr.io, not registry-1.docker.io.
	req := httptest.NewRequest("GET", "/registry/docker-hub/library/nginx/manifests/latest", nil)
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
	h := newCachedRegistryHandler(backend, map[string]string{
		"docker-hub": "registry-1.docker.io",
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
	h := newCachedRegistryHandler(backend, nil, nil)

	req := httptest.NewRequest("GET", "/registry/docker-hub/library/nginx/manifests/latest", nil)
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
	h := newCachedRegistryHandler(backend, nil, nil)

	req := httptest.NewRequest("POST", "/registry/docker-hub/library/nginx/manifests/latest", nil)
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
	h := newCachedRegistryHandler(backend, map[string]string{
		"ghcr": "ghcr.io",
	}, nil)

	req := httptest.NewRequest("GET", "/registry/ghcr/myorg/myimage/blobs/sha256:abc123", nil)
	req = withScope(req, registryTestScope("ghcr.io"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if want := "/v2/ghcr/myorg/myimage/blobs/sha256:abc123"; capturedPath != want {
		t.Fatalf("expected backend path %q, got %q", want, capturedPath)
	}
}

func TestCachedRegistryHandlerHeadAllowed(t *testing.T) {
	t.Parallel()

	var capturedMethod string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		w.WriteHeader(http.StatusOK)
	})
	h := newCachedRegistryHandler(backend, map[string]string{
		"docker-hub": "registry-1.docker.io",
	}, nil)

	req := httptest.NewRequest("HEAD", "/registry/docker-hub/library/nginx/manifests/latest", nil)
	req = withScope(req, registryTestScope("registry-1.docker.io"))
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
	h := newCachedRegistryHandler(backend, nil, nil)

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
