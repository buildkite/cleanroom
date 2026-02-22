package gitgateway

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
	"time"

	"github.com/buildkite/cleanroom/internal/policy"
)

func TestCopyProxyHeadersIncludesContentEncoding(t *testing.T) {
	t.Parallel()

	src := http.Header{}
	src.Set("Content-Encoding", "gzip")
	dst := http.Header{}
	copyProxyHeaders(dst, src)

	if got, want := dst.Get("Content-Encoding"), "gzip"; got != want {
		t.Fatalf("unexpected content-encoding: got %q want %q", got, want)
	}
}

func TestParseGatewayPath(t *testing.T) {
	t.Parallel()

	req, ok := parseGatewayPath("/git/github.com/org/repo.git/git-upload-pack")
	if !ok {
		t.Fatal("expected path to parse")
	}
	if got, want := req.RepoKey(), "org/repo"; got != want {
		t.Fatalf("unexpected repo key: got %q want %q", got, want)
	}

	scoped, ok := parseGatewayPath("/git/scope-abc/github.com/org/repo.git/git-upload-pack")
	if !ok {
		t.Fatal("expected scoped path to parse")
	}
	if got, want := scoped.Scope, "scope-abc"; got != want {
		t.Fatalf("unexpected scope: got %q want %q", got, want)
	}
}

func TestGatewayRejectsDisallowedRepo(t *testing.T) {
	t.Parallel()

	h := newHandler(&policy.GitPolicy{
		Enabled:      true,
		Source:       "upstream",
		AllowedHosts: []string{"github.com"},
		AllowedRepos: []string{"org/allowed"},
	}, nil, "", "https", &http.Client{Timeout: 3 * time.Second})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/git/github.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusForbidden; got != want {
		t.Fatalf("unexpected status: got %d want %d", got, want)
	}
	if body := rr.Body.String(); !strings.Contains(body, "repository_not_allowed") {
		t.Fatalf("expected repository_not_allowed body, got %q", body)
	}
}

func TestGatewayProxiesUpstreamInUpstreamMode(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/org/repo.git/info/refs"; got != want {
			t.Fatalf("unexpected upstream path: got %q want %q", got, want)
		}
		if got, want := r.URL.Query().Get("service"), "git-upload-pack"; got != want {
			t.Fatalf("unexpected upstream service query: got %q want %q", got, want)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	host := strings.TrimPrefix(upstream.URL, "http://")
	h := newHandler(&policy.GitPolicy{
		Enabled:      true,
		Source:       "upstream",
		AllowedHosts: []string{host},
	}, nil, "", "http", &http.Client{Timeout: 3 * time.Second})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/git/"+host+"/org/repo.git/info/refs?service=git-upload-pack", nil)
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusOK; got != want {
		t.Fatalf("unexpected status: got %d want %d", got, want)
	}
	if got, want := strings.TrimSpace(rr.Body.String()), "ok"; got != want {
		t.Fatalf("unexpected response body: got %q want %q", got, want)
	}
}

func TestStartRequiresHostWhenEnabled(t *testing.T) {
	t.Parallel()

	_, err := Start(context.Background(), Config{GitPolicy: &policy.GitPolicy{Enabled: true}})
	if err == nil {
		t.Fatal("expected Start to fail when listen host is missing")
	}
}

func TestGatewayUsesLocalMirrorInHostMirrorMode(t *testing.T) {
	t.Parallel()

	mirrorRoot := t.TempDir()
	repoPath := filepath.Join(mirrorRoot, "github.com", "org", "repo.git")
	if err := os.MkdirAll(filepath.Dir(repoPath), 0o755); err != nil {
		t.Fatalf("mkdir mirror parent: %v", err)
	}
	initCmd := exec.Command("git", "init", "--bare", repoPath)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("init bare mirror: %v\n%s", err, out)
	}

	h := newHandler(&policy.GitPolicy{
		Enabled:      true,
		Source:       "host_mirror",
		AllowedHosts: []string{"github.com"},
		AllowedRepos: []string{"org/repo"},
	}, nil, mirrorRoot, "https", &http.Client{Timeout: 3 * time.Second})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/git/github.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusOK; got != want {
		t.Fatalf("unexpected status: got %d want %d", got, want)
	}
	if got := rr.Header().Get("Content-Type"); !strings.Contains(got, "application/x-git-upload-pack-advertisement") {
		t.Fatalf("unexpected content type: %q", got)
	}
	if body := rr.Body.String(); !strings.Contains(body, "# service=git-upload-pack") {
		t.Fatalf("expected upload-pack service header in body, got %q", body)
	}
}

func TestScopedPolicyResolver(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "scoped-ok")
	}))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")

	h := newHandler(nil, func(scope string) (*policy.GitPolicy, bool) {
		if scope != "scope-ok" {
			return nil, false
		}
		return &policy.GitPolicy{Enabled: true, Source: "upstream", AllowedHosts: []string{host}}, true
	}, "", "http", &http.Client{Timeout: 3 * time.Second})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/git/scope-ok/"+host+"/org/repo.git/info/refs?service=git-upload-pack", nil)
	h.ServeHTTP(rr, req)
	if got, want := rr.Code, http.StatusOK; got != want {
		t.Fatalf("unexpected status: got %d want %d", got, want)
	}

	rrMissing := httptest.NewRecorder()
	reqMissing := httptest.NewRequest(http.MethodGet, "/git/scope-missing/"+host+"/org/repo.git/info/refs?service=git-upload-pack", nil)
	h.ServeHTTP(rrMissing, reqMissing)
	if got, want := rrMissing.Code, http.StatusForbidden; got != want {
		t.Fatalf("unexpected status for missing scope: got %d want %d", got, want)
	}
}
