package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

type staticCredentialProvider struct {
	tokens map[string]string
}

func (p *staticCredentialProvider) Resolve(_ context.Context, host string) (string, error) {
	return p.tokens[host], nil
}

func gitTestScope() *SandboxScope {
	return &SandboxScope{
		SandboxID: "sandbox-test",
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

func withScope(r *http.Request, scope *SandboxScope) *http.Request {
	ctx := context.WithValue(r.Context(), scopeContextKey, scope)
	return r.WithContext(ctx)
}

func TestGitHandlerHostNotAllowed(t *testing.T) {
	t.Parallel()

	h := newGitHandler(nil, nil)
	req := httptest.NewRequest("GET", "/git/evil.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, gitTestScope())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "host_not_allowed") {
		t.Fatalf("expected host_not_allowed in body, got %q", body)
	}
}

func TestGitHandlerReceivePackDenied(t *testing.T) {
	t.Parallel()

	h := newGitHandler(nil, nil)

	// GET info/refs with service=git-receive-pack
	req := httptest.NewRequest("GET", "/git/github.com/org/repo.git/info/refs?service=git-receive-pack", nil)
	req = withScope(req, gitTestScope())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for receive-pack info/refs, got %d", w.Code)
	}

	// POST git-receive-pack
	req = httptest.NewRequest("POST", "/git/github.com/org/repo.git/git-receive-pack", nil)
	req = withScope(req, gitTestScope())
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for receive-pack POST, got %d", w.Code)
	}
}

func TestGitHandlerMissingHost(t *testing.T) {
	t.Parallel()

	h := newGitHandler(nil, nil)

	req := httptest.NewRequest("GET", "/git/", nil)
	req = withScope(req, gitTestScope())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGitHandlerMissingRepoPath(t *testing.T) {
	t.Parallel()

	h := newGitHandler(nil, nil)

	req := httptest.NewRequest("GET", "/git/github.com", nil)
	req = withScope(req, gitTestScope())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGitHandlerProxiesUpstream(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("git-refs-data"))
	}))
	defer upstream.Close()

	// Extract host:port from upstream URL
	upstreamHost := strings.TrimPrefix(upstream.URL, "https://")

	scope := &SandboxScope{
		SandboxID: "sandbox-test",
		GuestIP:   "10.1.1.2",
		Policy: &policy.CompiledPolicy{
			Version:        1,
			NetworkDefault: "deny",
			Allow:          []policy.AllowRule{{Host: upstreamHost, Ports: []int{443}}},
		},
	}

	creds := &staticCredentialProvider{tokens: map[string]string{upstreamHost: "test-token"}}
	h := newGitHandler(creds, nil)
	h.client = upstream.Client()

	req := httptest.NewRequest("GET", "/git/"+upstreamHost+"/org/repo.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, scope)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if string(body) != "git-refs-data" {
		t.Fatalf("unexpected body: %q", string(body))
	}
}

func TestGitHandlerNoScope(t *testing.T) {
	t.Parallel()

	h := newGitHandler(nil, nil)
	req := httptest.NewRequest("GET", "/git/github.com/org/repo.git/info/refs", nil)
	// No scope in context
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestClassifyRequest(t *testing.T) {
	t.Parallel()

	h := &gitHandler{}
	tests := []struct {
		method  string
		path    string
		query   string
		wantErr bool
		wantAct string
	}{
		{"GET", "/org/repo.git/info/refs", "service=git-upload-pack", false, "info-refs"},
		{"GET", "/org/repo.git/info/refs", "service=git-receive-pack", true, ""},
		{"POST", "/org/repo.git/git-upload-pack", "", false, "upload-pack"},
		{"POST", "/org/repo.git/git-receive-pack", "", true, ""},
		{"GET", "/org/repo.git/HEAD", "", true, ""},
	}
	for _, tt := range tests {
		act, err := h.classifyRequest(tt.method, tt.path, tt.query)
		if tt.wantErr && err == nil {
			t.Errorf("classifyRequest(%s, %s, %s) expected error", tt.method, tt.path, tt.query)
		}
		if !tt.wantErr && err != nil {
			t.Errorf("classifyRequest(%s, %s, %s) unexpected error: %v", tt.method, tt.path, tt.query, err)
		}
		if !tt.wantErr && act != tt.wantAct {
			t.Errorf("classifyRequest(%s, %s, %s) = %q, want %q", tt.method, tt.path, tt.query, act, tt.wantAct)
		}
	}
}
