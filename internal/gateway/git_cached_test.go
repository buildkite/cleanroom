package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

func cachedGitTestScope() *SandboxScope {
	return &SandboxScope{
		SandboxID: "sandbox-cached-test",
		GuestIP:   "10.1.1.2",
		Policy: &policy.CompiledPolicy{
			Version:        1,
			NetworkDefault: "deny",
			Allow: []policy.AllowRule{
				{Host: "github.com", Ports: []int{443}},
			},
		},
	}
}

func TestCachedGitHandlerPolicyDeniesUnallowedHost(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called for denied host")
	})
	h := newCachedGitHandler(backend, nil)

	req := httptest.NewRequest("GET", "/git/evil.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, cachedGitTestScope())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if got := w.Header().Get(reasonCodeHeader); got != reasonHostNotAllowed {
		t.Fatalf("expected reason %s, got %q", reasonHostNotAllowed, got)
	}
}

func TestCachedGitHandlerRejectsReceivePack(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called for receive-pack")
	})
	h := newCachedGitHandler(backend, nil)

	req := httptest.NewRequest("POST", "/git/github.com/org/repo.git/git-receive-pack", nil)
	req = withScope(req, cachedGitTestScope())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestCachedGitHandlerNoScope(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called without scope")
	})
	h := newCachedGitHandler(backend, nil)

	req := httptest.NewRequest("GET", "/git/github.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestCachedGitHandlerStripsGitPrefixAndDelegates(t *testing.T) {
	t.Parallel()

	var capturedPath string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	h := newCachedGitHandler(backend, nil)

	req := httptest.NewRequest("GET", "/git/github.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, cachedGitTestScope())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if want := "/github.com/org/repo.git/info/refs"; capturedPath != want {
		t.Fatalf("expected backend path %q, got %q", want, capturedPath)
	}
}

func TestCachedGitHandlerUploadPackDelegates(t *testing.T) {
	t.Parallel()

	var capturedPath, capturedMethod string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedMethod = r.Method
		w.WriteHeader(http.StatusOK)
	})
	h := newCachedGitHandler(backend, nil)

	req := httptest.NewRequest("POST", "/git/github.com/org/repo.git/git-upload-pack", nil)
	req = withScope(req, cachedGitTestScope())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if want := "/github.com/org/repo.git/git-upload-pack"; capturedPath != want {
		t.Fatalf("expected backend path %q, got %q", want, capturedPath)
	}
	if capturedMethod != "POST" {
		t.Fatalf("expected POST, got %s", capturedMethod)
	}
}
