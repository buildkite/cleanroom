package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
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
