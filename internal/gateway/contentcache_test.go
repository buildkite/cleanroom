package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

func registryTestScopeWithRules(rules ...policy.AllowRule) *SandboxScope {
	return &SandboxScope{
		SandboxID: "sandbox-registry-test",
		GuestIP:   "10.1.1.2",
		Policy: &policy.CompiledPolicy{
			Version:        1,
			NetworkDefault: "deny",
			Allow:          rules,
		},
	}
}

func TestConfiguredOCIMirrorHostsOnlyIncludesConfiguredNonDockerHubRegistries(t *testing.T) {
	t.Parallel()

	got := configuredOCIMirrorHosts(map[string]string{
		"ghcr.io":            "https://ghcr.io",
		" public.ecr.aws ":   " https://public.ecr.aws ",
		"docker.io":          "https://registry-1.docker.io",
		"index.docker.io":    "https://registry-1.docker.io",
		"registry.internal/": "https://registry.internal",
		"":                   "https://example.invalid",
	})

	want := []string{"ghcr.io", "public.ecr.aws"}
	if len(got) != len(want) {
		t.Fatalf("unexpected mirror host count: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected mirror host at %d: got %q want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

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
	sourceHost, sourcePort, err := registryHostPort(redirectSource.URL)
	if err != nil {
		t.Fatalf("parse redirect source: %v", err)
	}

	client := newOCIContentCacheHTTPClient(nil)
	req, err := http.NewRequest(http.MethodGet, redirectSource.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	scope := registryTestScopeWithRules(
		policy.AllowRule{Host: sourceHost, Ports: []int{sourcePort}},
		policy.AllowRule{Host: targetHost, Ports: []int{targetPort}},
	)
	req = withScope(req, scope)

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

func TestNewOCIContentCacheHTTPClientRejectsDisallowedDirectRequests(t *testing.T) {
	t.Parallel()

	requested := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requested = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	client := newOCIContentCacheHTTPClient(nil)
	req, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req = withScope(req, registryTestScope("docker.io"))

	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected direct request to disallowed target to fail")
	}
	if requested {
		t.Fatal("expected disallowed direct target to remain unrequested")
	}
}

func TestNewOCIContentCacheHTTPClientAllowsMappedInitialRequestAgainstResolvedPolicyTarget(t *testing.T) {
	t.Parallel()

	requested := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requested = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamHost, upstreamPort, err := registryHostPort(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream target: %v", err)
	}

	client := newOCIContentCacheHTTPClient(nil)
	req, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req = withScope(req, registryTestScope("docker.io"))
	req = req.Clone(withOCIUpstreamPolicy(req.Context(), "docker.io", 443, upstreamHost, upstreamPort))

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("mapped request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, resp.StatusCode)
	}
	if !requested {
		t.Fatal("expected mapped upstream target to be requested")
	}
}

func TestNewOCIContentCacheHTTPClientRejectsDisallowedRedirectsFromMappedUpstream(t *testing.T) {
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
	req = withScope(req, registryTestScope("docker.io"))
	req = req.Clone(withOCIUpstreamPolicy(req.Context(), "docker.io", 443, sourceHost, sourcePort))

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
