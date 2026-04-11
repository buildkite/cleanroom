package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewGitContentCacheHTTPClientDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	redirected := false
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected = true
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()

	redirectSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	defer redirectSource.Close()

	client := newGitContentCacheHTTPClient(nil)
	resp, err := client.Get(redirectSource.URL)
	if err != nil {
		t.Fatalf("get redirected source: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected %d, got %d", http.StatusFound, resp.StatusCode)
	}
	if redirected {
		t.Fatal("expected redirected target to remain unrequested")
	}
}

func TestNewOCIContentCacheHTTPClientFollowsAllowedRedirects(t *testing.T) {
	t.Parallel()

	redirected := false
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected = true
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()

	redirectSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	defer redirectSource.Close()

	targetHost, targetPort, err := registryHostPort(redirectTarget.URL)
	if err != nil {
		t.Fatalf("parse redirect target: %v", err)
	}

	client := newOCIContentCacheHTTPClient(nil)
	req, err := http.NewRequest(http.MethodGet, redirectSource.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req = withScope(req, registryTestScopeWithRule(targetHost, targetPort))

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get redirected source: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, resp.StatusCode)
	}
	if !redirected {
		t.Fatal("expected redirected target to be requested")
	}
}

func TestNewOCIContentCacheHTTPClientRejectsDisallowedRedirects(t *testing.T) {
	t.Parallel()

	redirected := false
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected = true
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()

	redirectSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	defer redirectSource.Close()

	sourceHost, sourcePort, err := registryHostPort(redirectSource.URL)
	if err != nil {
		t.Fatalf("parse redirect source: %v", err)
	}

	client := newOCIContentCacheHTTPClient(nil)
	req, err := http.NewRequest(http.MethodGet, redirectSource.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req = withScope(req, registryTestScopeWithRule(sourceHost, sourcePort))

	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected redirect to disallowed target to fail")
	}
	if redirected {
		t.Fatal("expected disallowed redirect target to remain unrequested")
	}
}
