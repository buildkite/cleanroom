package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

type stubGitHostHandlerProvider struct {
	handler http.Handler
	err     error
	host    string
}

func (s *stubGitHostHandlerProvider) GitHandlerForHost(host string) (http.Handler, error) {
	s.host = host
	if s.err != nil {
		return nil, s.err
	}
	return s.handler, nil
}

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
	h := newCachedGitHandler(&stubGitHostHandlerProvider{handler: backend}, nil, nil)

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
	h := newCachedGitHandler(&stubGitHostHandlerProvider{handler: backend}, nil, nil)

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
	h := newCachedGitHandler(&stubGitHostHandlerProvider{handler: backend}, nil, nil)

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
	provider := &stubGitHostHandlerProvider{handler: backend}
	h := newCachedGitHandler(provider, nil, nil)

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
	if provider.host != "github.com" {
		t.Fatalf("expected git cache host github.com, got %q", provider.host)
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
	provider := &stubGitHostHandlerProvider{handler: backend}
	h := newCachedGitHandler(provider, nil, nil)

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
	if provider.host != "github.com" {
		t.Fatalf("expected git cache host github.com, got %q", provider.host)
	}
}

func TestCachedGitHandlerRejectsUnconfiguredCacheHost(t *testing.T) {
	t.Parallel()

	provider := &stubGitHostHandlerProvider{
		err: errors.New("not configured"),
	}
	h := newCachedGitHandler(provider, nil, nil)

	req := httptest.NewRequest("GET", "/git/github.enterprise.test/org/repo.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, &SandboxScope{
		SandboxID: "sandbox-cached-test",
		GuestIP:   "10.1.1.2",
		Policy: &policy.CompiledPolicy{
			Version:        1,
			NetworkDefault: "deny",
			Allow: []policy.AllowRule{
				{Host: "github.enterprise.test", Ports: []int{443}},
			},
		},
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if got := w.Header().Get(reasonCodeHeader); got != reasonHostNotAllowed {
		t.Fatalf("expected reason %s, got %q", reasonHostNotAllowed, got)
	}
}

func TestCachedGitHandlerFallsBackForNonDotGitRemotes(t *testing.T) {
	t.Parallel()

	var capturedPath, capturedQuery string
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	})
	provider := &stubGitHostHandlerProvider{
		handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("cache handler should not be used for non-.git remotes")
		}),
	}
	h := newCachedGitHandler(provider, fallback, nil)

	req := httptest.NewRequest("GET", "/git/github.com/org/repo/info/refs?service=git-upload-pack", nil)
	req = withScope(req, cachedGitTestScope())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if want := "/git/github.com/org/repo/info/refs"; capturedPath != want {
		t.Fatalf("expected fallback path %q, got %q", want, capturedPath)
	}
	if want := "service=git-upload-pack"; capturedQuery != want {
		t.Fatalf("expected fallback query %q, got %q", want, capturedQuery)
	}
	if provider.host != "" {
		t.Fatalf("expected cache host lookup to be skipped, got %q", provider.host)
	}
}
