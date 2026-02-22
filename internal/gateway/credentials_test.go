package gateway

import (
	"context"
	"testing"
)

func TestEnvCredentialProviderResolvesKnownHost(t *testing.T) {
	t.Setenv("CLEANROOM_GITHUB_TOKEN", "ghp_test123")
	p := NewEnvCredentialProvider()

	token, err := p.Resolve(context.Background(), "github.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if token != "ghp_test123" {
		t.Fatalf("expected ghp_test123, got %q", token)
	}
}

func TestEnvCredentialProviderUnknownHostReturnsEmpty(t *testing.T) {
	t.Parallel()

	p := NewEnvCredentialProvider()
	token, err := p.Resolve(context.Background(), "unknown.example.com")
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

	token, err := p.Resolve(context.Background(), "github.com")
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

	token, err := p.Resolve(context.Background(), "GitHub.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if token != "ghp_token" {
		t.Fatalf("expected ghp_token, got %q", token)
	}
}
