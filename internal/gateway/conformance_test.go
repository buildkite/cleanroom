package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/charmbracelet/log"
)

// --- Cross-sandbox isolation ---

func TestCrossSandboxIsolation(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()

	policyA := &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
	}
	policyB := &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow:          []policy.AllowRule{{Host: "gitlab.com", Ports: []int{443}}},
	}

	if err := reg.Register("10.0.0.1", "sandbox-A", policyA); err != nil {
		t.Fatalf("register A: %v", err)
	}
	if err := reg.Register("10.0.0.2", "sandbox-B", policyB); err != nil {
		t.Fatalf("register B: %v", err)
	}

	// Registry-level: each IP returns its own scope.
	scopeA, ok := reg.Lookup("10.0.0.1")
	if !ok || scopeA.SandboxID != "sandbox-A" {
		t.Fatalf("lookup 10.0.0.1: expected sandbox-A, got %+v", scopeA)
	}
	if scopeA.Policy != policyA {
		t.Fatal("sandbox-A policy mismatch")
	}

	scopeB, ok := reg.Lookup("10.0.0.2")
	if !ok || scopeB.SandboxID != "sandbox-B" {
		t.Fatalf("lookup 10.0.0.2: expected sandbox-B, got %+v", scopeB)
	}
	if scopeB.Policy != policyB {
		t.Fatal("sandbox-B policy mismatch")
	}

	// HTTP-level: identity middleware injects the correct scope per source IP.
	srv := &Server{registry: reg}
	var captured *SandboxScope
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured, _ = ScopeFromContext(r.Context())
	})
	handler := srv.identityMiddleware(inner)

	// Request from sandbox A's IP.
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.0.0.1:50000"
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if captured == nil || captured.SandboxID != "sandbox-A" {
		t.Fatalf("request from 10.0.0.1: expected sandbox-A scope, got %+v", captured)
	}

	// Request from sandbox B's IP.
	req = httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.0.0.2:50001"
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if captured == nil || captured.SandboxID != "sandbox-B" {
		t.Fatalf("request from 10.0.0.2: expected sandbox-B scope, got %+v", captured)
	}

	// Request from unknown IP gets no scope (403).
	req = httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.0.0.99:50002"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("request from unregistered IP: expected 403, got %d", w.Code)
	}
}

func TestCrossSandboxPolicyEnforcement(t *testing.T) {
	t.Parallel()

	// Sandbox A allows host-a.test, sandbox B allows host-b.test.
	// Denied hosts get 403; allowed hosts pass policy but hit an unreachable
	// upstream, so they get 502 (bad gateway). Both outcomes confirm the
	// correct policy was applied per source IP.
	reg := NewRegistry()
	if err := reg.Register("10.0.0.1", "sandbox-A", &policy.CompiledPolicy{
		Version: 1, NetworkDefault: "deny",
		Allow: []policy.AllowRule{{Host: "host-a.test", Ports: []int{443}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register("10.0.0.2", "sandbox-B", &policy.CompiledPolicy{
		Version: 1, NetworkDefault: "deny",
		Allow: []policy.AllowRule{{Host: "host-b.test", Ports: []int{443}}},
	}); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0",
		Registry:   reg,
	})

	tests := []struct {
		name       string
		remoteAddr string
		path       string
		wantCode   int
	}{
		{"A accesses host-a (allowed)", "10.0.0.1:50000", "/git/host-a.test/org/repo.git/info/refs?service=git-upload-pack", http.StatusBadGateway},
		{"A accesses host-b (denied)", "10.0.0.1:50000", "/git/host-b.test/org/repo.git/info/refs?service=git-upload-pack", http.StatusForbidden},
		{"B accesses host-b (allowed)", "10.0.0.2:50001", "/git/host-b.test/org/repo.git/info/refs?service=git-upload-pack", http.StatusBadGateway},
		{"B accesses host-a (denied)", "10.0.0.2:50001", "/git/host-a.test/org/repo.git/info/refs?service=git-upload-pack", http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			req.RemoteAddr = tt.remoteAddr
			w := httptest.NewRecorder()
			srv.httpServer.Handler.ServeHTTP(w, req)
			if w.Code != tt.wantCode {
				t.Fatalf("expected %d, got %d (body: %s)", tt.wantCode, w.Code, w.Body.String())
			}
		})
	}
}

// --- Credential leak test ---

func TestCredentialNotLeakedToClient(t *testing.T) {
	t.Parallel()

	const secretToken = "ghp_SUPERSECRET_TOKEN_12345"

	// Upstream server: verifies it receives the token, responds with benign body.
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer "+secretToken {
			t.Errorf("upstream: expected Authorization header with token, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("refs-response"))
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "https://")
	scope := &SandboxScope{
		SandboxID: "sandbox-leak-test",
		GuestIP:   "10.1.1.2",
		Policy: &policy.CompiledPolicy{
			Version: 1, NetworkDefault: "deny",
			Allow: []policy.AllowRule{{Host: upstreamHost, Ports: []int{443}}},
		},
	}

	creds := &staticCredentialProvider{tokens: map[string]string{upstreamHost: secretToken}}

	// Capture log output.
	var logBuf bytes.Buffer
	logger := log.NewWithOptions(&logBuf, log.Options{})

	h := newGitHandler(creds, logger)
	h.client = upstream.Client()

	req := httptest.NewRequest("GET", "/git/"+upstreamHost+"/org/repo.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, scope)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	// Response body must not contain the token.
	body, _ := io.ReadAll(w.Body)
	if strings.Contains(string(body), secretToken) {
		t.Fatal("credential token leaked in response body")
	}

	// Response headers must not contain the token.
	for key, vals := range w.Header() {
		for _, v := range vals {
			if strings.Contains(v, secretToken) {
				t.Fatalf("credential token leaked in response header %s: %s", key, v)
			}
		}
	}

	// Log output must not contain the token.
	if strings.Contains(logBuf.String(), secretToken) {
		t.Fatalf("credential token leaked in log output: %s", logBuf.String())
	}
}
