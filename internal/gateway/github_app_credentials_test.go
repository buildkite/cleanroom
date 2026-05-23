package gateway

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ccgit "github.com/buildkite/content-cache/protocol/git"
)

func TestGitHubAppCredentialProviderResolvesGitHubRemote(t *testing.T) {
	_, privateKeyPEM := testGitHubAppPrivateKeyPEM(t)
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)

	var tokenRequests []githubAppTokenRequestBody
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want %q", r.Method, http.MethodPost)
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/app/installations/67890/access_tokens" {
			t.Errorf("path = %q, want access_tokens endpoint", r.URL.Path)
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		var body githubAppTokenRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode token request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		tokenRequests = append(tokenRequests, body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(githubAppTokenResponseBody{
			Token:     "installation-token",
			ExpiresAt: now.Add(time.Hour),
		})
	}))
	t.Cleanup(api.Close)

	auth, err := ccgit.NewGitHubAppAuth(ccgit.GitHubAppAuthConfig{
		AppID:          "12345",
		InstallationID: "67890",
		PrivateKey:     privateKeyPEM,
		TokenScope:     ccgit.GitHubAppTokenScopeRequestedRepo,
	}, ccgit.WithGitHubAppAPIURL(api.URL), ccgit.WithGitHubAppClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("new auth: %v", err)
	}

	provider, err := NewGitHubAppCredentialProvider(auth, []string{"buildkite/"})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	header, err := provider.Resolve(context.Background(), "https://github.com/buildkite/cleanroom.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	wantHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:installation-token"))
	if header != wantHeader {
		t.Fatalf("authorization header = %q, want %q", header, wantHeader)
	}
	if len(tokenRequests) != 1 {
		t.Fatalf("token requests = %d, want 1", len(tokenRequests))
	}
	if got, want := tokenRequests[0].Repositories, []string{"cleanroom"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("repositories = %v, want %v", got, want)
	}
}

func TestGitHubAppCredentialProviderSkipsNonGitHubRemote(t *testing.T) {
	_, privateKeyPEM := testGitHubAppPrivateKeyPEM(t)
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)

	var tokenRequests int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenRequests++
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(api.Close)

	auth, err := ccgit.NewGitHubAppAuth(ccgit.GitHubAppAuthConfig{
		AppID:          "12345",
		InstallationID: "67890",
		PrivateKey:     privateKeyPEM,
		TokenScope:     ccgit.GitHubAppTokenScopeRequestedRepo,
	}, ccgit.WithGitHubAppAPIURL(api.URL), ccgit.WithGitHubAppClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("new auth: %v", err)
	}

	provider, err := NewGitHubAppCredentialProvider(auth, []string{"buildkite/"})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	header, err := provider.Resolve(context.Background(), "https://gitlab.com/buildkite/cleanroom.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if header != "" {
		t.Fatalf("expected empty header for non-GitHub remote, got %q", header)
	}
	if tokenRequests != 0 {
		t.Fatalf("token requests = %d, want 0", tokenRequests)
	}
}

func TestGitHubAppCredentialProviderSkipsOutOfScopeGitHubRemote(t *testing.T) {
	_, privateKeyPEM := testGitHubAppPrivateKeyPEM(t)
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)

	var tokenRequests int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenRequests++
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(api.Close)

	auth, err := ccgit.NewGitHubAppAuth(ccgit.GitHubAppAuthConfig{
		AppID:          "12345",
		InstallationID: "67890",
		PrivateKey:     privateKeyPEM,
		TokenScope:     ccgit.GitHubAppTokenScopeRequestedRepo,
	}, ccgit.WithGitHubAppAPIURL(api.URL), ccgit.WithGitHubAppClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("new auth: %v", err)
	}

	provider, err := NewGitHubAppCredentialProvider(auth, []string{"buildkite/cleanroom"})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	header, err := provider.Resolve(context.Background(), "https://github.com/other/private-repo.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if header != "" {
		t.Fatalf("expected empty header for out-of-scope remote, got %q", header)
	}
	if tokenRequests != 0 {
		t.Fatalf("token requests = %d, want 0", tokenRequests)
	}
}

func TestGitHubAppCredentialProviderFallsBackForOutOfScopeRemote(t *testing.T) {
	_, privateKeyPEM := testGitHubAppPrivateKeyPEM(t)
	auth, err := ccgit.NewGitHubAppAuth(ccgit.GitHubAppAuthConfig{
		AppID:          "12345",
		InstallationID: "67890",
		PrivateKey:     privateKeyPEM,
		TokenScope:     ccgit.GitHubAppTokenScopeRequestedRepo,
	})
	if err != nil {
		t.Fatalf("new auth: %v", err)
	}
	githubProvider, err := NewGitHubAppCredentialProvider(auth, []string{"buildkite/cleanroom"})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	provider := NewChainCredentialProvider(
		githubProvider,
		&staticCredentialProvider{headers: map[string]string{"https://github.com/other/private-repo.git": "Bearer fallback-token"}},
	)

	header, err := provider.Resolve(context.Background(), "https://github.com/other/private-repo.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if header != "Bearer fallback-token" {
		t.Fatalf("expected fallback header, got %q", header)
	}
}

func TestGitHubAppCredentialProviderSkipsNonRepoGitHubURL(t *testing.T) {
	_, privateKeyPEM := testGitHubAppPrivateKeyPEM(t)
	auth, err := ccgit.NewGitHubAppAuth(ccgit.GitHubAppAuthConfig{
		AppID:          "12345",
		InstallationID: "67890",
		PrivateKey:     privateKeyPEM,
		TokenScope:     ccgit.GitHubAppTokenScopeRequestedRepo,
	})
	if err != nil {
		t.Fatalf("new auth: %v", err)
	}

	provider, err := NewGitHubAppCredentialProvider(auth, []string{"buildkite/"})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	header, err := provider.Resolve(context.Background(), "https://github.com/buildkite/")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if header != "" {
		t.Fatalf("expected empty header for non-repo GitHub URL, got %q", header)
	}
}

func TestGitHubAppCredentialProviderFailsClosedBeforeFallback(t *testing.T) {
	_, privateKeyPEM := testGitHubAppPrivateKeyPEM(t)
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "github unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(api.Close)

	auth, err := ccgit.NewGitHubAppAuth(ccgit.GitHubAppAuthConfig{
		AppID:          "12345",
		InstallationID: "67890",
		PrivateKey:     privateKeyPEM,
		TokenScope:     ccgit.GitHubAppTokenScopeRequestedRepo,
	}, ccgit.WithGitHubAppAPIURL(api.URL), ccgit.WithGitHubAppClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("new auth: %v", err)
	}

	githubProvider, err := NewGitHubAppCredentialProvider(auth, []string{"buildkite/"})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	provider := NewChainCredentialProvider(
		githubProvider,
		&staticCredentialProvider{headers: map[string]string{"https://github.com/buildkite/cleanroom.git": "Bearer fallback-token"}},
	)
	header, err := provider.Resolve(context.Background(), "https://github.com/buildkite/cleanroom.git")
	if err == nil {
		t.Fatal("expected GitHub App token error")
	}
	if header != "" {
		t.Fatalf("expected empty header on error, got %q", header)
	}
	if strings.Contains(err.Error(), "fallback-token") {
		t.Fatalf("error leaked fallback token: %v", err)
	}
}

func TestNewGitHubAppCredentialProviderFromConfig(t *testing.T) {
	t.Run("unconfigured", func(t *testing.T) {
		provider, err := NewGitHubAppCredentialProviderFromConfig(GitHubAppCredentialConfig{})
		if err != nil {
			t.Fatalf("from config: %v", err)
		}
		if provider != nil {
			t.Fatalf("expected nil provider, got %#v", provider)
		}
	})

	t.Run("private key file", func(t *testing.T) {
		_, privateKeyPEM := testGitHubAppPrivateKeyPEM(t)
		privateKeyPath := filepath.Join(t.TempDir(), "github-app.pem")
		if err := os.WriteFile(privateKeyPath, []byte(privateKeyPEM), 0o600); err != nil {
			t.Fatalf("write private key: %v", err)
		}

		provider, err := NewGitHubAppCredentialProviderFromConfig(GitHubAppCredentialConfig{
			AppID:          "12345",
			InstallationID: "67890",
			PrivateKeyFile: privateKeyPath,
			RepoPrefixes:   []string{" buildkite/ ", ""},
		})
		if err != nil {
			t.Fatalf("from config: %v", err)
		}
		if provider == nil {
			t.Fatal("expected configured provider")
		}
	})

	t.Run("partial config fails startup", func(t *testing.T) {
		_, err := NewGitHubAppCredentialProviderFromConfig(GitHubAppCredentialConfig{
			AppID:        "12345",
			RepoPrefixes: []string{"buildkite/"},
		})
		if err == nil {
			t.Fatal("expected partial config error")
		}
		if !strings.Contains(err.Error(), "installation_id") {
			t.Fatalf("expected missing installation ID in error, got %v", err)
		}
	})
}

func TestNormalizeGitHubAppRepoPrefixes(t *testing.T) {
	t.Parallel()

	got, err := normalizeGitHubAppRepoPrefixes([]string{
		"Buildkite/",
		"github.com/buildkite/cleanroom.git",
		"https://github.com/Other/Repo",
		"buildkite/",
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := []string{"buildkite/", "buildkite/cleanroom", "other/repo"}
	if len(got) != len(want) {
		t.Fatalf("prefix count = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("prefix[%d] = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

type githubAppTokenRequestBody struct {
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

type githubAppTokenResponseBody struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func testGitHubAppPrivateKeyPEM(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	block := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}
	return key, string(pem.EncodeToMemory(block))
}
