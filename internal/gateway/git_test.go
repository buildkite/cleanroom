package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

type staticCredentialProvider struct {
	headers map[string]string
}

func (p *staticCredentialProvider) Resolve(_ context.Context, remoteURL string) (string, error) {
	return p.headers[remoteURL], nil
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
	if got := w.Header().Get("X-Cleanroom-Reason-Code"); got != "host_not_allowed" {
		t.Fatalf("expected X-Cleanroom-Reason-Code=host_not_allowed, got %q", got)
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

	creds := &staticCredentialProvider{headers: map[string]string{"https://" + upstreamHost + "/org/repo.git": "Bearer test-token"}}
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

func TestGitHandlerForwardsEndToEndHeadersAndStripsGatewayHeaders(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Bearer test-token"; got != want {
			t.Fatalf("unexpected Authorization header: got %q want %q", got, want)
		}
		if got, want := r.Header.Get("User-Agent"), "git/2.49.0"; got != want {
			t.Fatalf("unexpected User-Agent header: got %q want %q", got, want)
		}
		if got, want := r.Header.Get("Git-Protocol"), "version=2"; got != want {
			t.Fatalf("unexpected Git-Protocol header: got %q want %q", got, want)
		}
		if got, want := r.Header.Get("Pragma"), "no-cache"; got != want {
			t.Fatalf("unexpected Pragma header: got %q want %q", got, want)
		}
		if got, want := r.Header.Get("Cache-Control"), "no-cache"; got != want {
			t.Fatalf("unexpected Cache-Control header: got %q want %q", got, want)
		}
		if got := r.Header.Get(ScopeTokenHeader); got != "" {
			t.Fatalf("did not expect scope token header upstream, got %q", got)
		}
		if got := r.Header.Get("Proxy-Authorization"); got != "" {
			t.Fatalf("did not expect proxy authorization header upstream, got %q", got)
		}
		if got := r.Header.Get("Connection"); got != "" {
			t.Fatalf("did not expect Connection header upstream, got %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if got, want := string(body), "request-payload"; got != want {
			t.Fatalf("unexpected proxied body: got %q want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("packfile"))
	}))
	defer upstream.Close()

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

	creds := &staticCredentialProvider{headers: map[string]string{"https://" + upstreamHost + "/org/repo.git": "Bearer test-token"}}
	h := newGitHandler(creds, nil)
	h.client = upstream.Client()

	req := httptest.NewRequest("POST", "/git/"+upstreamHost+"/org/repo.git/git-upload-pack", strings.NewReader("request-payload"))
	req = withScope(req, scope)
	req.Header.Set("Authorization", "Basic should-not-leak")
	req.Header.Set("User-Agent", "git/2.49.0")
	req.Header.Set("Git-Protocol", "version=2")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set(ScopeTokenHeader, "scope-token")
	req.Header.Set("Connection", "keep-alive, X-Debug-Header")
	req.Header.Set("X-Debug-Header", "strip-me")
	req.Header.Set("Proxy-Authorization", "Basic also-strip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
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

func TestGitHandlerServesMirrorToRealGitClient(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	runGitCommand(t, workDir, "init")
	runGitCommand(t, workDir, "config", "user.email", "test@example.com")
	runGitCommand(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGitCommand(t, workDir, "add", "README.md")
	runGitCommand(t, workDir, "commit", "-m", "initial")

	mirrorDir := filepath.Join(t.TempDir(), "mirror.git")
	runGitCommand(t, "", "clone", "--mirror", workDir, mirrorDir)
	headSHA := strings.TrimSpace(runGitCommand(t, mirrorDir, "rev-parse", "HEAD"))

	scope := gitTestScope()
	mirrorStore := &staticMirrorStore{mirrorDir: mirrorDir}
	handler := newGitHandler(nil, nil)
	handler.mirrors = mirrorStore

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = withScope(r, scope)
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()

	remoteURL := server.URL + "/git/github.com/org/repo.git"
	cmd := exec.Command("git", "ls-remote", remoteURL, "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git ls-remote failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), headSHA) {
		t.Fatalf("expected ls-remote output %q to contain HEAD %s", out, headSHA)
	}
	if got, want := mirrorStore.gotRemoteURL, "https://github.com/org/repo.git"; got != want {
		t.Fatalf("unexpected canonical remote URL: got %q want %q", got, want)
	}
}

type staticMirrorStore struct {
	mirrorDir    string
	gotRemoteURL string
}

func (s *staticMirrorStore) EnsureMirror(_ context.Context, remoteURL string) (string, error) {
	s.gotRemoteURL = remoteURL
	return s.mirrorDir, nil
}

func (s *staticMirrorStore) EnsureMirrorContains(_ context.Context, remoteURL, _ string) error {
	s.gotRemoteURL = remoteURL
	return nil
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

func TestUploadPackConfigArgsEnablePartialCloneFilter(t *testing.T) {
	t.Parallel()

	args := uploadPackConfigArgs()
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "uploadpack.allowFilter=true") {
		t.Fatalf("expected upload-pack config args to enable filter support, got %q", joined)
	}
}
