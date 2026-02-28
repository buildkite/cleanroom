package darwinvz

import (
	"strings"
	"testing"
)

func TestEvaluateNetworkPolicyRequiresDenyDefault(t *testing.T) {
	warn, err := evaluateNetworkPolicy("allow", 0)
	if err == nil {
		t.Fatal("expected error for non-deny network default")
	}
	if warn != "" {
		t.Fatalf("expected empty warning when validation fails, got %q", warn)
	}
	if !strings.Contains(err.Error(), "deny-by-default") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEvaluateNetworkPolicyWarnsWhenAllowEntriesPresent(t *testing.T) {
	warn, err := evaluateNetworkPolicy("deny", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(warn, "ignores sandbox.network.allow") {
		t.Fatalf("unexpected warning: %q", warn)
	}
}

func TestEvaluateNetworkPolicyAcceptsDenyWithNoAllowEntries(t *testing.T) {
	warn, err := evaluateNetworkPolicy("deny", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if warn != "" {
		t.Fatalf("expected no warning, got %q", warn)
	}
}
