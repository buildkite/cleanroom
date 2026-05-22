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
		handlers:    make(map[string]goProxyScopedHandler),
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

	handlerA, err := cache.GoProxyHandlerForPolicy(policyA)
	if err != nil {
		t.Fatalf("handler A: %v", err)
	}
	if _, err := cache.GoProxyHandlerForPolicy(policyB); err != nil {
		t.Fatalf("handler B: %v", err)
	}
	reusedA, err := cache.GoProxyHandlerForPolicy(policyA)
	if err != nil {
		t.Fatalf("reused handler A: %v", err)
	}
	if reusedA != handlerA {
		t.Fatal("expected policy A handler to be reused before eviction")
	}
	if _, err := cache.GoProxyHandlerForPolicy(policyC); err != nil {
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
	if !slices.Equal(closed, []string{"policy-b"}) {
		t.Fatalf("expected policy-b closer to run, got %v", closed)
	}
}
