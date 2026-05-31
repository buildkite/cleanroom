package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/buildkite/cleanroom/internal/gatewayauth"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracetest "go.opentelemetry.io/otel/sdk/trace/tracetest"
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

func TestGitHandlerRequiresOwnerWhenConfigured(t *testing.T) {
	t.Parallel()

	h := newGitHandlerWithMirrors(nil, nil, nil, true)
	req := httptest.NewRequest("GET", "/git/github.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, gitTestScope())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if got := w.Header().Get(reasonCodeHeader); got != reasonGatewayAuthDenied {
		t.Fatalf("expected reason %s, got %q", reasonGatewayAuthDenied, got)
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

func TestGitHandlerRejectsEmbeddedPortHost(t *testing.T) {
	t.Parallel()

	h := newGitHandler(nil, nil)
	h.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("upstream transport should not be called for an invalid route host")
		return nil, nil
	})}

	scope := &SandboxScope{
		SandboxID: "sandbox-test",
		GuestIP:   "10.1.1.2",
		Policy: &policy.CompiledPolicy{
			Version:        1,
			NetworkDefault: "deny",
			Allow: []policy.AllowRule{
				{Host: "127.0.0.1", Ports: []int{443}},
				{Host: "127.0.0.1:8443", Ports: []int{443}},
			},
		},
	}

	req := httptest.NewRequest("GET", "/git/127.0.0.1:8443/org/repo.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, scope)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestGitHandlerRejectsEscapedSlashInRouteHost(t *testing.T) {
	t.Parallel()

	h := newGitHandlerWithMirrors(nil, nil, nil, true)
	h.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("upstream transport should not be called for an invalid route host")
		return nil, nil
	})}

	scope := gitTestScope()
	scope.GatewayScope = gatewayauth.ScopeMetadata{
		Owner: gatewayauth.Owner{
			PrincipalID: "oidc:test:alice",
			Scope:       "repo:org/repo",
		},
		Authorization: gatewayauth.Authorization{
			GitRepoPrefixes: []string{"github.com/org/"},
		},
	}

	req := httptest.NewRequest("GET", "/git/github.com%2forg/../private.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, scope)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestSplitGitRequestPathRejectsUnsafeHosts(t *testing.T) {
	t.Parallel()

	for _, rawPath := range []string{
		"/git/github.com:8443/org/repo.git/info/refs",
		"/git/user@github.com/org/repo.git/info/refs",
		"/git/github.com%3a8443/org/repo.git/info/refs",
		"/git/github.com%zz/org/repo.git/info/refs",
		"/git/[::1]/org/repo.git/info/refs",
		"/git/github.com\\evil/org/repo.git/info/refs",
		"/git/ github.com/org/repo.git/info/refs",
	} {
		if host, repoPath, err := splitGitRequestPath(rawPath); err == nil {
			t.Fatalf("splitGitRequestPath(%q) = host %q repo %q, want error", rawPath, host, repoPath)
		}
	}
}

func TestGitHandlerProxiesUpstream(t *testing.T) {
	t.Parallel()

	h := newGitHandler(&staticCredentialProvider{headers: map[string]string{
		"https://github.com/org/repo.git": "Bearer test-token",
	}}, nil)
	h.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %q", auth)
		}
		if got, want := r.URL.String(), "https://github.com/org/repo.git/info/refs?service=git-upload-pack"; got != want {
			t.Fatalf("unexpected upstream URL: got %q want %q", got, want)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/x-git-upload-pack-advertisement"}},
			Body:       io.NopCloser(strings.NewReader("git-refs-data")),
		}, nil
	})}

	req := httptest.NewRequest("GET", "/git/github.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, gitTestScope())
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

func TestGitHandlerAuditLogIncludesTraceCorrelation(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := log.NewWithOptions(&logBuf, log.Options{Formatter: log.JSONFormatter})
	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider()
	tracerProvider.RegisterSpanProcessor(recorder)
	defer func() {
		_ = tracerProvider.Shutdown(context.Background())
	}()

	srv := &Server{tracerProvider: tracerProvider}
	h := newGitHandler(nil, logger)
	handler := srv.tracingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
	}))

	req := httptest.NewRequest("GET", "/git/evil.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, gitTestScope())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logBuf.Bytes()), &payload); err != nil {
		t.Fatalf("expected json gateway log, got error: %v\noutput=%s", err, logBuf.String())
	}
	if got, want := payload["action"], gatewayActionDeny; got != want {
		t.Fatalf("unexpected action: got %#v want %#v", got, want)
	}
	if got, want := payload["reason_code"], reasonHostNotAllowed; got != want {
		t.Fatalf("unexpected reason_code: got %#v want %#v", got, want)
	}
	if got, want := payload[observability.LogFieldSandboxID], "sandbox-test"; got != want {
		t.Fatalf("unexpected sandbox_id: got %#v want %#v", got, want)
	}
	if _, ok := payload[observability.LogFieldTraceID]; !ok {
		t.Fatalf("expected trace_id in log payload: %#v", payload)
	}
	if _, ok := payload[observability.LogFieldSpanID]; !ok {
		t.Fatalf("expected span_id in log payload: %#v", payload)
	}
}

func TestGitHandlerForwardsEndToEndHeadersAndStripsGatewayHeaders(t *testing.T) {
	t.Parallel()

	h := newGitHandler(&staticCredentialProvider{headers: map[string]string{
		"https://github.com/org/repo.git": "Bearer test-token",
	}}, nil)
	h.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if got, want := r.Header.Get("Authorization"), "Bearer test-token"; got != want {
			t.Fatalf("unexpected Authorization header: got %q want %q", got, want)
		}
		if got, want := r.URL.Path, "/org/repo.git/git-upload-pack"; got != want {
			t.Fatalf("unexpected upstream path: got %q want %q", got, want)
		}
		if got := r.URL.RawQuery; got != "" {
			t.Fatalf("unexpected upstream query: got %q want empty", got)
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
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/x-git-upload-pack-result"}},
			Body:       io.NopCloser(strings.NewReader("packfile")),
		}, nil
	})}

	req := httptest.NewRequest("POST", "/git/github.com/org/repo.git/git-upload-pack", strings.NewReader("request-payload"))
	req = withScope(req, gitTestScope())
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

func TestGitHandlerUpstreamTransportFailureMarksSpanDenied(t *testing.T) {
	t.Parallel()

	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider()
	tracerProvider.RegisterSpanProcessor(recorder)
	defer func() {
		_ = tracerProvider.Shutdown(context.Background())
	}()

	h := newGitHandler(nil, nil)
	h.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: i/o timeout")
	})}

	srv := &Server{tracerProvider: tracerProvider}
	handler := srv.tracingMiddleware(h)

	req := httptest.NewRequest("GET", "/git/github.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	req = withScope(req, gitTestScope())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
	if got := w.Header().Get(reasonCodeHeader); got != reasonUpstreamError {
		t.Fatalf("expected %s=%s, got %q", reasonCodeHeader, reasonUpstreamError, got)
	}

	spans := recorder.Ended()
	var gatewaySpan sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.Name() == "cleanroom.gateway.git.request" {
			gatewaySpan = span
			break
		}
	}
	if gatewaySpan == nil {
		t.Fatalf("expected gateway span, got spans %#v", spans)
	}
	if got := spanAttributeValue(gatewaySpan, "cleanroom.gateway.action"); got != "deny" {
		t.Fatalf("expected cleanroom.gateway.action=deny, got %q", got)
	}
	if got := spanAttributeValue(gatewaySpan, "cleanroom.reason_code"); got != reasonUpstreamError {
		t.Fatalf("expected cleanroom.reason_code=%s, got %q", reasonUpstreamError, got)
	}
}

func TestGitHandlerMirrorOversizedUploadPackReturnsRequestTooLarge(t *testing.T) {
	oldLimit := maxUploadPackRequestBytes
	maxUploadPackRequestBytes = 8
	t.Cleanup(func() {
		maxUploadPackRequestBytes = oldLimit
	})

	h := newGitHandler(nil, nil)
	h.mirrors = &staticMirrorStore{mirrorDir: t.TempDir()}

	req := httptest.NewRequest("POST", "/git/github.com/org/repo.git/git-upload-pack", strings.NewReader("012345678"))
	req = withScope(req, gitTestScope())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d (body: %s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get(reasonCodeHeader); got != reasonInvalidRequest {
		t.Fatalf("expected %s=%s, got %q", reasonCodeHeader, reasonInvalidRequest, got)
	}
	if body := w.Body.String(); !strings.Contains(body, errUploadPackRequestTooLarge.Error()) {
		t.Fatalf("expected body to include oversize error, got %q", body)
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

func TestServeMirrorUploadPackWritesCommandOutput(t *testing.T) {
	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	if err := os.WriteFile(gitPath, []byte("#!/bin/sh\ncat >/dev/null\nprintf 'pack-result'\n"), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	req := httptest.NewRequest(http.MethodPost, "/git/github.com/org/repo.git/git-upload-pack", strings.NewReader("request-payload"))
	resp := httptest.NewRecorder()
	if err := serveMirrorUploadPack(req, resp, t.TempDir()); err != nil {
		t.Fatalf("serveMirrorUploadPack returned error: %v", err)
	}
	if got, want := resp.Header().Get("Content-Type"), "application/x-git-upload-pack-result"; got != want {
		t.Fatalf("unexpected content type: got %q want %q", got, want)
	}
	if got, want := resp.Body.String(), "pack-result"; got != want {
		t.Fatalf("unexpected response body: got %q want %q", got, want)
	}
}

func TestGitHandlerDoesNotAppendErrorAfterMirrorUploadPackStreamStarts(t *testing.T) {
	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	if err := os.WriteFile(gitPath, []byte("#!/bin/sh\ncat >/dev/null\nprintf 'partial-pack'\nprintf 'broken mirror\\n' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	handler := newGitHandler(nil, nil)
	handler.mirrors = &staticMirrorStore{mirrorDir: t.TempDir()}

	req := httptest.NewRequest(http.MethodPost, "/git/github.com/org/repo.git/git-upload-pack", strings.NewReader("request-payload"))
	req = withScope(req, gitTestScope())
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if got, want := resp.Code, http.StatusOK; got != want {
		t.Fatalf("unexpected response code after partial stream: got %d want %d", got, want)
	}
	if got, want := resp.Body.String(), "partial-pack"; got != want {
		t.Fatalf("unexpected response body: got %q want %q", got, want)
	}
	if strings.Contains(resp.Body.String(), reasonUpstreamError) || strings.Contains(resp.Body.String(), "broken mirror") {
		t.Fatalf("gateway error was appended to streamed pack response: %q", resp.Body.String())
	}
}

type staticMirrorStore struct {
	mirrorDir    string
	gotRemoteURL string
}

func (s *staticMirrorStore) MirrorPath(remoteURL string) (string, error) {
	s.gotRemoteURL = remoteURL
	return s.mirrorDir, nil
}

func (s *staticMirrorStore) EnsureMirror(_ context.Context, remoteURL string) (string, error) {
	s.gotRemoteURL = remoteURL
	return s.mirrorDir, nil
}

func (s *staticMirrorStore) RefreshMirror(_ context.Context, remoteURL string) (string, error) {
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

func TestReadUploadPackBodyRejectsOversizedPlainBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/git/github.com/org/repo.git/git-upload-pack", strings.NewReader("012345678"))

	_, err := readUploadPackBodyWithLimit(req, 8)
	if !errors.Is(err, errUploadPackRequestTooLarge) {
		t.Fatalf("readUploadPackBody error = %v, want %v", err, errUploadPackRequestTooLarge)
	}
}

func TestReadUploadPackBodyRejectsOversizedGzipExpansion(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write([]byte("012345678")); err != nil {
		t.Fatalf("write gzip body: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/git/github.com/org/repo.git/git-upload-pack", &compressed)
	req.Header.Set("Content-Encoding", "gzip")

	_, err := readUploadPackBodyWithLimit(req, 8)
	if !errors.Is(err, errUploadPackRequestTooLarge) {
		t.Fatalf("readUploadPackBody error = %v, want %v", err, errUploadPackRequestTooLarge)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func spanAttributeValue(span sdktrace.ReadOnlySpan, key string) string {
	for _, attr := range span.Attributes() {
		if string(attr.Key) != key {
			continue
		}
		if attr.Value.Type() == attribute.STRING {
			return attr.Value.AsString()
		}
		return attr.Value.Emit()
	}
	return ""
}
