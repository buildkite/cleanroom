package gitgateway

import (
	"strings"
	"testing"
)

func TestBuildRewriteRulesForScopeSortsAndDeduplicatesHosts(t *testing.T) {
	t.Parallel()

	rules, err := BuildRewriteRulesForScope("http://127.0.0.1:18080", "", []string{"GitHub.com", "github.com", "gitlab.com"})
	if err != nil {
		t.Fatalf("BuildRewriteRulesForScope returned error: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("unexpected rule count: got %d want 2", len(rules))
	}
	if got, want := rules[0].InsteadOf, "https://github.com/"; got != want {
		t.Fatalf("unexpected first insteadOf: got %q want %q", got, want)
	}
	if got, want := rules[0].BaseURL, "http://127.0.0.1:18080/git/github.com/"; got != want {
		t.Fatalf("unexpected first base URL: got %q want %q", got, want)
	}
	if got, want := rules[1].InsteadOf, "https://gitlab.com/"; got != want {
		t.Fatalf("unexpected second insteadOf: got %q want %q", got, want)
	}
}

func TestBuildRewriteRulesForScopeRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	_, err := BuildRewriteRulesForScope("", "", []string{"github.com"})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected relay URL required error, got %v", err)
	}

	_, err = BuildRewriteRulesForScope("http://127.0.0.1:18080", "", nil)
	if err == nil || !strings.Contains(err.Error(), "at least one allowed host") {
		t.Fatalf("expected allowed host error, got %v", err)
	}

	_, err = BuildRewriteRulesForScope("http://127.0.0.1:18080", "", []string{"owner/repo"})
	if err == nil || !strings.Contains(err.Error(), "hostname") {
		t.Fatalf("expected hostname error, got %v", err)
	}
}

func TestBuildRewriteRulesForScope(t *testing.T) {
	t.Parallel()

	rules, err := BuildRewriteRulesForScope("http://10.99.0.1:18080", "sandbox-scope", []string{"github.com"})
	if err != nil {
		t.Fatalf("BuildRewriteRulesForScope returned error: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("unexpected rule count: got %d want 1", len(rules))
	}
	if got, want := rules[0].BaseURL, "http://10.99.0.1:18080/git/sandbox-scope/github.com/"; got != want {
		t.Fatalf("unexpected scoped base URL: got %q want %q", got, want)
	}
}
