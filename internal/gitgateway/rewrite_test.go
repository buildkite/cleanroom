package gitgateway

import (
	"strings"
	"testing"
)

func TestBuildRewriteRulesSortsAndDeduplicatesHosts(t *testing.T) {
	t.Parallel()

	rules, err := BuildRewriteRules("http://127.0.0.1:18080", []string{"GitHub.com", "github.com", "gitlab.com"})
	if err != nil {
		t.Fatalf("BuildRewriteRules returned error: %v", err)
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

func TestBuildRewriteRulesRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	_, err := BuildRewriteRules("", []string{"github.com"})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected relay URL required error, got %v", err)
	}

	_, err = BuildRewriteRules("http://127.0.0.1:18080", nil)
	if err == nil || !strings.Contains(err.Error(), "at least one allowed host") {
		t.Fatalf("expected allowed host error, got %v", err)
	}

	_, err = BuildRewriteRules("http://127.0.0.1:18080", []string{"owner/repo"})
	if err == nil || !strings.Contains(err.Error(), "hostname") {
		t.Fatalf("expected hostname error, got %v", err)
	}
}
