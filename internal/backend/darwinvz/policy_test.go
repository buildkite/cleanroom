package darwinvz

import (
	"strings"
	"testing"
)

func TestEvaluateNetworkPolicyRequiresDenyDefault(t *testing.T) {
	warn, err := evaluateNetworkPolicyForRun("allow", 0, false)
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

func TestEvaluateNetworkPolicyForDoctorWarnsWhenAllowEntriesPresentWithoutHostFilter(t *testing.T) {
	warn, err := evaluateNetworkPolicyForDoctor("deny", 2, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(warn, "ignores sandbox.network.allow") {
		t.Fatalf("unexpected warning: %q", warn)
	}
}

func TestEvaluateNetworkPolicyForRunFailsWhenAllowEntriesPresentWithoutHostFilter(t *testing.T) {
	warn, err := evaluateNetworkPolicyForRun("deny", 2, false)
	if err == nil {
		t.Fatal("expected error when allow entries are present without host filter")
	}
	if warn != "" {
		t.Fatalf("expected no warning, got %q", warn)
	}
	if !strings.Contains(err.Error(), "requires host-side egress filtering") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEvaluateNetworkPolicyAcceptsDenyWithNoAllowEntries(t *testing.T) {
	warn, err := evaluateNetworkPolicyForRun("deny", 0, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if warn != "" {
		t.Fatalf("expected no warning, got %q", warn)
	}
}

func TestEvaluateNetworkPolicyAcceptsAllowEntriesWhenHostFilterIsEnabled(t *testing.T) {
	warn, err := evaluateNetworkPolicyForRun("deny", 2, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if warn != "" {
		t.Fatalf("expected no warning, got %q", warn)
	}
}
