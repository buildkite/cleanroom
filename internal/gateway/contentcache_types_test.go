package gateway

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
	ccgit "github.com/buildkite/content-cache/protocol/git"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type comparableHandler struct{}

func (comparableHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

func TestCredentialInjectorReturnsCredentialErrors(t *testing.T) {
	t.Parallel()

	called := false
	rt := &credentialInjector{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
			}, nil
		}),
		credentials: failingCredentialProvider{},
	}

	req, err := http.NewRequest(http.MethodGet, "https://github.com/buildkite/cleanroom.git/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	_, err = rt.RoundTrip(req)
	if err == nil {
		t.Fatal("expected credential error")
	}
	if !strings.Contains(err.Error(), "resolve upstream credentials") {
		t.Fatalf("expected upstream credential context, got %v", err)
	}
	if called {
		t.Fatal("base round tripper should not be called")
	}
}

func TestCredentialInjectorSkipsCredentialResolutionForHTTPUpstream(t *testing.T) {
	t.Parallel()

	called := false
	rt := &credentialInjector{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
			}, nil
		}),
		credentials: failingCredentialProvider{},
	}

	req, err := http.NewRequest(http.MethodGet, "http://registry.internal:5000/v2/library/alpine/manifests/latest", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if !called {
		t.Fatal("expected base round tripper to be called")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestContentCacheGitBasicAuthProviderResolvesBasicCredential(t *testing.T) {
	t.Parallel()

	header := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:installation-token"))
	provider := newContentCacheGitBasicAuthProvider(&staticCredentialProvider{headers: map[string]string{
		"https://github.com/buildkite/cleanroom.git": header,
	}})

	username, password, err := provider.BasicAuth(context.Background(), ccgit.RepoRef{
		Host:     "github.com",
		RepoPath: "buildkite/cleanroom",
	})
	if err != nil {
		t.Fatalf("basic auth: %v", err)
	}
	if username != "x-access-token" || password != "installation-token" {
		t.Fatalf("basic auth = (%q, %q), want x-access-token and installation-token", username, password)
	}
}

func TestContentCacheGitBasicAuthProviderIgnoresNonBasicCredential(t *testing.T) {
	t.Parallel()

	provider := newContentCacheGitBasicAuthProvider(&staticCredentialProvider{headers: map[string]string{
		"https://github.com/buildkite/cleanroom.git": "Bearer host-token",
	}})

	username, password, err := provider.BasicAuth(context.Background(), ccgit.RepoRef{
		Host:     "github.com",
		RepoPath: "buildkite/cleanroom",
	})
	if err != nil {
		t.Fatalf("basic auth: %v", err)
	}
	if username != "" || password != "" {
		t.Fatalf("expected non-Basic credential to be ignored, got (%q, %q)", username, password)
	}
}

func TestGitContentCacheUpstreamFailsBeforeHTTPWhenCredentialsFail(t *testing.T) {
	t.Parallel()

	called := false
	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
			}, nil
		}),
	}
	upstream := newGitContentCacheUpstream(client, slog.Default(), failingCredentialProvider{})

	_, _, err := upstream.FetchInfoRefs(context.Background(), ccgit.RepoRef{
		Host:     "github.com",
		RepoPath: "buildkite/cleanroom",
	}, "")
	if err == nil {
		t.Fatal("expected credential error")
	}
	if !strings.Contains(err.Error(), "resolve upstream credentials") {
		t.Fatalf("expected upstream credential context, got %v", err)
	}
	if called {
		t.Fatal("upstream HTTP client should not be called")
	}
}

func TestPolicyValidatingRoundTripperUsesPinnedPolicyWithoutScope(t *testing.T) {
	t.Parallel()

	called := false
	rt := &policyValidatingRoundTripper{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
			}, nil
		}),
		policy: &policy.CompiledPolicy{
			NetworkDefault: "deny",
			Allow: []policy.AllowRule{{
				Host:  "storage.googleapis.com",
				Ports: []int{443},
			}},
		},
	}

	req, err := http.NewRequest(http.MethodGet, "https://storage.googleapis.com/example/module.zip", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !called {
		t.Fatal("expected base round tripper to be called")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestPolicyValidatingRoundTripperRejectsScopeLessRequestsWithoutPinnedPolicy(t *testing.T) {
	t.Parallel()

	rt := &policyValidatingRoundTripper{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("base round tripper should not be called")
			return nil, nil
		}),
	}

	req, err := http.NewRequest(http.MethodGet, "https://proxy.golang.org/github.com/pkg/errors/@v/list", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	if _, err := rt.RoundTrip(req); err == nil {
		t.Fatal("expected missing scope validation error")
	}
}

func TestGoProxyHandlerForPolicyEvictsLeastRecentlyUsedHandler(t *testing.T) {
	t.Parallel()

	var closed []string
	cache := &ContentCache{}
	cache.goProxy = goProxyHandlerEntry{
		handlers:    make(map[string]*goProxyScopedHandler),
		maxHandlers: 2,
		buildHandler: func(compiled *policy.CompiledPolicy) (goProxyScopedHandler, error) {
			key := compiled.Hash
			return goProxyScopedHandler{
				handler: &comparableHandler{},
				closer: closeFunc(func() {
					closed = append(closed, key)
				}),
			}, nil
		},
	}

	policyA := &policy.CompiledPolicy{Hash: "policy-a"}
	policyB := &policy.CompiledPolicy{Hash: "policy-b"}
	policyC := &policy.CompiledPolicy{Hash: "policy-c"}

	handlerA, releaseA, err := cache.GoProxyHandlerForPolicy(policyA)
	if err != nil {
		t.Fatalf("handler A: %v", err)
	}
	_, releaseB, err := cache.GoProxyHandlerForPolicy(policyB)
	if err != nil {
		t.Fatalf("handler B: %v", err)
	}
	reusedA, releaseReusedA, err := cache.GoProxyHandlerForPolicy(policyA)
	if err != nil {
		t.Fatalf("reused handler A: %v", err)
	}
	if reusedA != handlerA {
		t.Fatal("expected policy A handler to be reused before eviction")
	}
	_, releaseC, err := cache.GoProxyHandlerForPolicy(policyC)
	if err != nil {
		t.Fatalf("handler C: %v", err)
	}

	if got, want := len(cache.goProxy.handlers), 2; got != want {
		t.Fatalf("expected %d cached handlers, got %d", want, got)
	}
	if _, ok := cache.goProxy.handlers["policy-a"]; !ok {
		t.Fatal("expected policy-a to remain cached after recent reuse")
	}
	if _, ok := cache.goProxy.handlers["policy-c"]; !ok {
		t.Fatal("expected policy-c to be cached")
	}
	if _, ok := cache.goProxy.handlers["policy-b"]; ok {
		t.Fatal("expected policy-b to be evicted")
	}
	if len(closed) != 0 {
		t.Fatalf("expected active policy-b handler to stay open until release, got %v", closed)
	}
	releaseB()
	if !slices.Equal(closed, []string{"policy-b"}) {
		t.Fatalf("expected policy-b closer to run, got %v", closed)
	}
	releaseA()
	releaseReusedA()
	releaseC()
}

func TestFetchHandlerForPolicyEvictsLeastRecentlyUsedHandler(t *testing.T) {
	t.Parallel()

	var closed []string
	cache := &ContentCache{}
	cache.fetch = fetchHandlerEntry{
		handlers:    make(map[string]*fetchScopedHandler),
		maxHandlers: 2,
		buildHandler: func(compiled *policy.CompiledPolicy) (fetchScopedHandler, error) {
			key := compiled.Hash
			return fetchScopedHandler{
				handler: &comparableHandler{},
				closer: closeFunc(func() {
					closed = append(closed, key)
				}),
			}, nil
		},
	}

	policyA := &policy.CompiledPolicy{Hash: "policy-a"}
	policyB := &policy.CompiledPolicy{Hash: "policy-b"}
	policyC := &policy.CompiledPolicy{Hash: "policy-c"}

	handlerA, releaseA, err := cache.FetchHandlerForPolicy(policyA)
	if err != nil {
		t.Fatalf("handler A: %v", err)
	}
	_, releaseB, err := cache.FetchHandlerForPolicy(policyB)
	if err != nil {
		t.Fatalf("handler B: %v", err)
	}
	reusedA, releaseReusedA, err := cache.FetchHandlerForPolicy(policyA)
	if err != nil {
		t.Fatalf("reused handler A: %v", err)
	}
	if reusedA != handlerA {
		t.Fatal("expected policy A handler to be reused before eviction")
	}
	_, releaseC, err := cache.FetchHandlerForPolicy(policyC)
	if err != nil {
		t.Fatalf("handler C: %v", err)
	}

	if got, want := len(cache.fetch.handlers), 2; got != want {
		t.Fatalf("expected %d cached handlers, got %d", want, got)
	}
	if _, ok := cache.fetch.handlers["policy-a"]; !ok {
		t.Fatal("expected policy-a to remain cached after recent reuse")
	}
	if _, ok := cache.fetch.handlers["policy-c"]; !ok {
		t.Fatal("expected policy-c to be cached")
	}
	if _, ok := cache.fetch.handlers["policy-b"]; ok {
		t.Fatal("expected policy-b to be evicted")
	}
	if len(closed) != 0 {
		t.Fatalf("expected active policy-b handler to stay open until release, got %v", closed)
	}
	releaseB()
	if !slices.Equal(closed, []string{"policy-b"}) {
		t.Fatalf("expected policy-b closer to run, got %v", closed)
	}
	releaseA()
	releaseReusedA()
	releaseC()
}

func TestOCIHandlerForPrefixEvictsLeastRecentlyUsedHandler(t *testing.T) {
	t.Parallel()

	var closed []string
	cache := &ContentCache{
		ociHandlers:    make(map[string]*ociHandlerEntry),
		maxOCIHandlers: 2,
		buildOCIHandler: func(prefix string) (ociHandlerEntry, error) {
			return ociHandlerEntry{
				handler: &comparableHandler{},
				closer: closeFunc(func() {
					closed = append(closed, prefix)
				}),
			}, nil
		},
	}

	handlerA, releaseA, err := cache.OCIHandlerForPrefix("registry-a.test")
	if err != nil {
		t.Fatalf("handler A: %v", err)
	}
	_, releaseB, err := cache.OCIHandlerForPrefix("registry-b.test")
	if err != nil {
		t.Fatalf("handler B: %v", err)
	}
	reusedA, releaseReusedA, err := cache.OCIHandlerForPrefix("registry-a.test")
	if err != nil {
		t.Fatalf("reused handler A: %v", err)
	}
	if reusedA != handlerA {
		t.Fatal("expected registry-a handler to be reused before eviction")
	}
	_, releaseC, err := cache.OCIHandlerForPrefix("registry-c.test")
	if err != nil {
		t.Fatalf("handler C: %v", err)
	}

	if got, want := len(cache.ociHandlers), 2; got != want {
		t.Fatalf("expected %d cached handlers, got %d", want, got)
	}
	if _, ok := cache.ociHandlers["registry-a.test"]; !ok {
		t.Fatal("expected registry-a to remain cached after recent reuse")
	}
	if _, ok := cache.ociHandlers["registry-c.test"]; !ok {
		t.Fatal("expected registry-c to be cached")
	}
	if _, ok := cache.ociHandlers["registry-b.test"]; ok {
		t.Fatal("expected registry-b to be evicted")
	}
	if len(closed) != 0 {
		t.Fatalf("expected active registry-b handler to stay open until release, got %v", closed)
	}
	releaseB()
	if !slices.Equal(closed, []string{"registry-b.test"}) {
		t.Fatalf("expected registry-b closer to run, got %v", closed)
	}
	releaseA()
	releaseReusedA()
	releaseC()
}
