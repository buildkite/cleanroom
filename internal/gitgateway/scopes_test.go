package gitgateway

import (
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

func TestScopeRegistrySetResolveDelete(t *testing.T) {
	t.Parallel()

	r := NewScopeRegistry()
	err := r.Set("scope-1", &policy.GitPolicy{Enabled: true, Source: "upstream", AllowedHosts: []string{"github.com"}})
	if err != nil {
		t.Fatalf("Set returned error: %v", err)
	}

	resolved, ok := r.Resolve("scope-1")
	if !ok || resolved == nil {
		t.Fatalf("expected scope to resolve")
	}
	if got, want := resolved.AllowedHosts[0], "github.com"; got != want {
		t.Fatalf("unexpected allowed host: got %q want %q", got, want)
	}

	resolved.AllowedHosts[0] = "example.com"
	resolvedAgain, ok := r.Resolve("scope-1")
	if !ok || resolvedAgain == nil || resolvedAgain.AllowedHosts[0] != "github.com" {
		t.Fatalf("expected Resolve to return defensive copy, got %+v", resolvedAgain)
	}

	if got, want := r.Delete("scope-1"), 0; got != want {
		t.Fatalf("unexpected remaining count after delete: got %d want %d", got, want)
	}
}
