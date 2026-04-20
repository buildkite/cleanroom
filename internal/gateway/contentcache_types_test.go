package gateway

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type comparableHandler struct{}

func (comparableHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

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
