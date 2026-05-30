package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/buildkite/cleanroom/internal/gatewayauth"
	"github.com/buildkite/cleanroom/internal/policy"
)

type stubGitHostHandlerProvider struct {
	handler http.Handler
	err     error
	host    string
	scope   string
}

func cachedGitOwnedScope(repoPrefixes ...string) *SandboxScope {
	scope := cachedGitTestScope()
	scope.GatewayScope = gatewayauth.ScopeMetadata{
		Owner: gatewayauth.Owner{
			PrincipalID: "oidc:test:alice",
			Scope:       "repo:buildkite/cleanroom",
		},
		Authorization: gatewayauth.Authorization{
			GitRepoPrefixes: repoPrefixes,
		},
	}
	return scope
}

func (s *stubGitHostHandlerProvider) GitHandlerForHost(host, cacheScope string) (http.Handler, error) {
	s.host = host
	s.scope = cacheScope
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

func withGatewayRequestObservability(r *http.Request) (*http.Request, *gatewayRequestObservability) {
	obs := &gatewayRequestObservability{}
	ctx := context.WithValue(r.Context(), gatewayRequestContextKey, obs)
	return r.WithContext(ctx), obs
}

func requireGatewayRequestDecision(t *testing.T, obs *gatewayRequestObservability, action, reason string) {
	t.Helper()

	if obs == nil {
		t.Fatal("missing gateway request observability")
	}
	if obs.action != action {
		t.Fatalf("expected gateway action %q, got %q", action, obs.action)
	}
	if obs.reasonCode != reason {
		t.Fatalf("expected gateway reason %q, got %q", reason, obs.reasonCode)
	}
}

func TestCachedGitHandlerPolicyDeniesUnallowedHost(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called for denied host")
	})
	h := newCachedGitHandler(&stubGitHostHandlerProvider{handler: backend}, nil, nil, false, nil)

	req := httptest.NewRequest("GET", "/git/evil.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, cachedGitTestScope())
	req, obs := withGatewayRequestObservability(req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if got := w.Header().Get(reasonCodeHeader); got != reasonHostNotAllowed {
		t.Fatalf("expected reason %s, got %q", reasonHostNotAllowed, got)
	}
	requireGatewayRequestDecision(t, obs, gatewayActionDeny, reasonHostNotAllowed)
}

func TestCachedGitHandlerRejectsReceivePack(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called for receive-pack")
	})
	h := newCachedGitHandler(&stubGitHostHandlerProvider{handler: backend}, nil, nil, false, nil)

	req := httptest.NewRequest("POST", "/git/github.com/org/repo.git/git-receive-pack", nil)
	req = withScope(req, cachedGitTestScope())
	req, obs := withGatewayRequestObservability(req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	requireGatewayRequestDecision(t, obs, gatewayActionDeny, reasonMethodNotAllowed)
}

func TestCachedGitHandlerNoScope(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called without scope")
	})
	h := newCachedGitHandler(&stubGitHostHandlerProvider{handler: backend}, nil, nil, false, nil)

	req := httptest.NewRequest("GET", "/git/github.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestCachedGitHandlerRequiresOwnerWhenConfigured(t *testing.T) {
	t.Parallel()

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("backend should not be called without owner")
	})
	h := newCachedGitHandler(&stubGitHostHandlerProvider{handler: backend}, nil, nil, true, nil)

	req := httptest.NewRequest("GET", "/git/github.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, cachedGitTestScope())
	req, obs := withGatewayRequestObservability(req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if got := w.Header().Get(reasonCodeHeader); got != reasonGatewayAuthDenied {
		t.Fatalf("expected reason %s, got %q", reasonGatewayAuthDenied, got)
	}
	requireGatewayRequestDecision(t, obs, gatewayActionDeny, reasonGatewayAuthDenied)
}

func TestCachedGitHandlerDeniesRepoOutsideGatewayEnvelope(t *testing.T) {
	t.Parallel()

	provider := &stubGitHostHandlerProvider{
		handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("backend should not be called for unauthorized repo")
		}),
	}
	h := newCachedGitHandler(provider, nil, nil, true, nil)

	req := httptest.NewRequest("GET", "/git/github.com/buildkite/private.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, cachedGitOwnedScope("github.com/buildkite/cleanroom"))
	req, obs := withGatewayRequestObservability(req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if provider.host != "" {
		t.Fatalf("expected cache handler lookup to be skipped, got %q", provider.host)
	}
	requireGatewayRequestDecision(t, obs, gatewayActionDeny, reasonGatewayAuthDenied)
}

func TestCachedGitHandlerAllowsRepoInGatewayEnvelope(t *testing.T) {
	t.Parallel()

	var called bool
	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := newCachedGitHandler(&stubGitHostHandlerProvider{handler: backend}, nil, nil, true, nil)

	req := httptest.NewRequest("GET", "/git/github.com/buildkite/cleanroom.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, cachedGitOwnedScope("github.com/buildkite/cleanroom"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !called {
		t.Fatal("expected cache handler to be called")
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
	h := newCachedGitHandler(provider, nil, nil, false, nil)

	req := httptest.NewRequest("GET", "/git/github.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, cachedGitTestScope())
	req, obs := withGatewayRequestObservability(req)
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
	requireGatewayRequestDecision(t, obs, gatewayActionAllow, reasonCached)
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
	h := newCachedGitHandler(provider, nil, nil, false, nil)

	req := httptest.NewRequest("POST", "/git/github.com/org/repo.git/git-upload-pack", nil)
	req = withScope(req, cachedGitTestScope())
	req, obs := withGatewayRequestObservability(req)
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
	requireGatewayRequestDecision(t, obs, gatewayActionAllow, reasonCached)
}

func TestCachedGitHandlerScopesCacheByCredentialAndOwnerAuthorization(t *testing.T) {
	t.Parallel()

	h := newCachedGitHandler(nil, nil, nil, true, &staticCredentialProvider{headers: map[string]string{
		"https://github.com/buildkite/cleanroom.git": "Basic dXNlcjpwYXNz",
	}})
	scopeA := cachedGitOwnedScope("github.com/buildkite/cleanroom")
	scopeA.Policy.Hash = "policy-a"
	keyA, cacheable, err := h.cacheScopeKey(context.Background(), scopeA, "github.com", "/buildkite/cleanroom.git/git-upload-pack", "upload-pack")
	if err != nil {
		t.Fatalf("cache scope A: %v", err)
	}
	if !cacheable {
		t.Fatal("expected Basic credential to be cacheable")
	}

	h.credentials = &staticCredentialProvider{headers: map[string]string{
		"https://github.com/buildkite/cleanroom.git": "Basic b3RoZXI6cGFzcw==",
	}}
	keyB, cacheable, err := h.cacheScopeKey(context.Background(), scopeA, "github.com", "/buildkite/cleanroom.git/git-upload-pack", "upload-pack")
	if err != nil {
		t.Fatalf("cache scope B: %v", err)
	}
	if !cacheable {
		t.Fatal("expected second Basic credential to be cacheable")
	}
	if keyA == keyB {
		t.Fatal("expected different credentials to use different git cache scopes")
	}

	h.credentials = &staticCredentialProvider{headers: map[string]string{
		"https://github.com/buildkite/cleanroom.git": "Basic dXNlcjpwYXNz",
	}}
	scopeB := cachedGitOwnedScope("github.com/buildkite/")
	scopeB.Policy.Hash = "policy-a"
	keyC, cacheable, err := h.cacheScopeKey(context.Background(), scopeB, "github.com", "/buildkite/cleanroom.git/git-upload-pack", "upload-pack")
	if err != nil {
		t.Fatalf("cache scope C: %v", err)
	}
	if !cacheable {
		t.Fatal("expected owner-authorized Basic credential to be cacheable")
	}
	if keyA == keyC {
		t.Fatal("expected different owner authorization envelopes to use different git cache scopes")
	}
}

func TestCachedGitHandlerFallsBackForUploadPackWithNonBasicCredential(t *testing.T) {
	t.Parallel()

	var fallbackCalled bool
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled = true
		if want := "/git/github.com/org/repo.git/git-upload-pack"; r.URL.Path != want {
			t.Fatalf("expected fallback path %q, got %q", want, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})
	provider := &stubGitHostHandlerProvider{
		handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("cache handler should not be used for non-Basic upload-pack credentials")
		}),
	}
	h := newCachedGitHandler(provider, fallback, nil, false, &staticCredentialProvider{headers: map[string]string{
		"https://github.com/org/repo.git": "Bearer host-token",
	}})

	req := httptest.NewRequest("POST", "/git/github.com/org/repo.git/git-upload-pack", nil)
	req = withScope(req, cachedGitTestScope())
	req, obs := withGatewayRequestObservability(req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !fallbackCalled {
		t.Fatal("expected fallback to be called")
	}
	if provider.host != "" {
		t.Fatalf("expected cache lookup to be skipped, got host %q", provider.host)
	}
	requireGatewayRequestDecision(t, obs, gatewayActionAllow, reasonFallback)
}

func TestCachedGitHandlerFallsBackForUnconfiguredCacheHost(t *testing.T) {
	t.Parallel()

	var capturedPath, capturedQuery string
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	})
	provider := &stubGitHostHandlerProvider{
		err: fmt.Errorf("%w: github.enterprise.test", errGitHostNotConfiguredForCaching),
	}
	h := newCachedGitHandler(provider, fallback, nil, false, nil)

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
	req, obs := withGatewayRequestObservability(req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if want := "/git/github.enterprise.test/org/repo.git/info/refs"; capturedPath != want {
		t.Fatalf("expected fallback path %q, got %q", want, capturedPath)
	}
	if want := "service=git-upload-pack"; capturedQuery != want {
		t.Fatalf("expected fallback query %q, got %q", want, capturedQuery)
	}
	if provider.host != "github.enterprise.test" {
		t.Fatalf("expected cache host lookup for github.enterprise.test, got %q", provider.host)
	}
	requireGatewayRequestDecision(t, obs, gatewayActionAllow, reasonFallback)
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
	h := newCachedGitHandler(provider, fallback, nil, false, nil)

	req := httptest.NewRequest("GET", "/git/github.com/org/repo/info/refs?service=git-upload-pack", nil)
	req = withScope(req, cachedGitTestScope())
	req, obs := withGatewayRequestObservability(req)
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
	requireGatewayRequestDecision(t, obs, gatewayActionAllow, reasonFallback)
}
