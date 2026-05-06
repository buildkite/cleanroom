package gateway

import (
	"context"
	"encoding/base64"
	"testing"
)

func TestEnvCredentialProviderResolvesGitHubAsBasic(t *testing.T) {
	t.Setenv("CLEANROOM_GITHUB_TOKEN", "ghp_test123")
	p := NewEnvCredentialProvider()

	token, err := p.Resolve(context.Background(), "https://github.com/buildkite/cleanroom.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghp_test123"))
	if token != want {
		t.Fatalf("expected %q, got %q", want, token)
	}
}

func TestEnvCredentialProviderResolvesGitLabAsBearer(t *testing.T) {
	t.Setenv("CLEANROOM_GITLAB_TOKEN", "glpat_test123")
	p := NewEnvCredentialProvider()

	token, err := p.Resolve(context.Background(), "https://gitlab.com/buildkite/cleanroom.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if token != "Bearer glpat_test123" {
		t.Fatalf("expected Bearer glpat_test123, got %q", token)
	}
}

func TestEnvCredentialProviderUnknownHostReturnsEmpty(t *testing.T) {
	t.Parallel()

	p := NewEnvCredentialProvider()
	token, err := p.Resolve(context.Background(), "https://unknown.example.com/org/repo.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if token != "" {
		t.Fatalf("expected empty token, got %q", token)
	}
}

func TestEnvCredentialProviderMissingEnvReturnsEmpty(t *testing.T) {
	// Explicitly unset to be sure
	t.Setenv("CLEANROOM_GITHUB_TOKEN", "")
	p := NewEnvCredentialProvider()

	token, err := p.Resolve(context.Background(), "https://github.com/buildkite/cleanroom.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if token != "" {
		t.Fatalf("expected empty token, got %q", token)
	}
}

func TestEnvCredentialProviderCaseInsensitive(t *testing.T) {
	t.Setenv("CLEANROOM_GITHUB_TOKEN", "ghp_token")
	p := NewEnvCredentialProvider()

	token, err := p.Resolve(context.Background(), "https://GitHub.com/buildkite/cleanroom.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghp_token"))
	if token != want {
		t.Fatalf("expected %q, got %q", want, token)
	}
}

func TestEnvCredentialProviderConfiguredHostsSorted(t *testing.T) {
	t.Setenv("CLEANROOM_GITHUB_TOKEN", "ghp_token")
	t.Setenv("CLEANROOM_GITLAB_TOKEN", "glpat_token")
	p := NewEnvCredentialProvider()

	hosts := p.ConfiguredHosts()
	if len(hosts) != 2 {
		t.Fatalf("expected 2 hosts, got %d (%v)", len(hosts), hosts)
	}
	if hosts[0] != "github.com" || hosts[1] != "gitlab.com" {
		t.Fatalf("expected sorted hosts [github.com gitlab.com], got %v", hosts)
	}
}

func TestGitCredentialFillProviderResolveUsesCanonicalRemoteURL(t *testing.T) {
	t.Parallel()

	var gotDir string
	var gotInput string
	provider := NewGitCredentialFillProvider("/tmp/repo", func(dir, input string) (string, error) {
		gotDir = dir
		gotInput = input
		return "protocol=https\nhost=github.com\npath=buildkite/cleanroom.git\nusername=git\npassword=secret\n", nil
	})

	header, err := provider.Resolve(context.Background(), "https://github.com/buildkite/cleanroom.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if gotDir != "/tmp/repo" {
		t.Fatalf("expected /tmp/repo lookup dir, got %q", gotDir)
	}
	wantInput := "protocol=https\nhost=github.com\npath=buildkite/cleanroom.git\n\n"
	if gotInput != wantInput {
		t.Fatalf("unexpected credential fill input: got %q want %q", gotInput, wantInput)
	}
	wantHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte("git:secret"))
	if header != wantHeader {
		t.Fatalf("unexpected authorization header: got %q want %q", header, wantHeader)
	}
}

func TestChainCredentialProviderFallsBackToGitCredentialFill(t *testing.T) {
	t.Setenv("CLEANROOM_GITHUB_TOKEN", "")

	provider := NewChainCredentialProvider(
		NewEnvCredentialProvider(),
		NewGitCredentialFillProvider("/tmp/repo", func(dir, input string) (string, error) {
			return "protocol=https\nhost=github.com\npath=buildkite/cleanroom.git\nusername=git\npassword=secret\n", nil
		}),
	)

	header, err := provider.Resolve(context.Background(), "https://github.com/buildkite/cleanroom.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	wantHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte("git:secret"))
	if header != wantHeader {
		t.Fatalf("unexpected authorization header: got %q want %q", header, wantHeader)
	}
}
