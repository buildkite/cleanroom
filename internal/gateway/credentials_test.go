package gateway

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvCredentialProviderResolvesKnownHost(t *testing.T) {
	t.Setenv("CLEANROOM_GITHUB_TOKEN", "ghp_test123")
	p := NewEnvCredentialProvider()

	token, err := p.Resolve(context.Background(), "https://github.com/buildkite/cleanroom.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if token != "Bearer ghp_test123" {
		t.Fatalf("expected Bearer ghp_test123, got %q", token)
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
	if token != "Bearer ghp_token" {
		t.Fatalf("expected Bearer ghp_token, got %q", token)
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

func TestGitCredentialFillProviderRejectsInjectedPathFields(t *testing.T) {
	t.Parallel()

	called := false
	provider := NewGitCredentialFillProvider("/tmp/repo", func(string, string) (string, error) {
		called = true
		return "", nil
	})

	_, err := provider.Resolve(context.Background(), "https://github.com/buildkite/cleanroom.git%0ahost=evil.example")
	if err == nil {
		t.Fatal("expected credential fill lookup to reject injected path field")
	}
	if !strings.Contains(err.Error(), "git credential fill") {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Fatal("expected git credential fill not to be invoked")
	}
}

func TestGitCredentialFillFromHostDisablesPrompts(t *testing.T) {
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, "env.txt")

	shimScript := "#!/bin/sh\n/usr/bin/env > " + envFile + "\nexit 1\n"
	shimPath := filepath.Join(tmpDir, "git")
	if err := os.WriteFile(shimPath, []byte(shimScript), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}

	t.Setenv("PATH", tmpDir)

	_, err := gitCredentialFillFromHost(tmpDir, "protocol=https\nhost=example.com\n\n")
	if err == nil {
		t.Fatal("expected non-nil error from shim exiting non-zero")
	}

	captured, readErr := os.ReadFile(envFile)
	if readErr != nil {
		t.Fatalf("read captured env: %v", readErr)
	}
	env := string(captured)

	if !strings.Contains(env, "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("GIT_TERMINAL_PROMPT=0 not found in captured env:\n%s", env)
	}
	if !strings.Contains(env, "GCM_INTERACTIVE=never") {
		t.Errorf("GCM_INTERACTIVE=never not found in captured env:\n%s", env)
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
