package darwinvz

import (
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
)

func TestEvaluateNetworkPolicyRequiresDenyDefault(t *testing.T) {
	warn, err := evaluateNetworkPolicyForRun("allow", 0, false)
	if err == nil {
		t.Fatal("expected error for non-deny network default")
	}
	if !strings.Contains(err.Error(), "deny-by-default") {
		t.Fatalf("unexpected error: %v", err)
	}
	if warn != "" {
		t.Fatalf("expected empty warning, got %q", warn)
	}
}

func TestEvaluateNetworkPolicyForDoctorAcceptsAllowEntries(t *testing.T) {
	warn, err := evaluateNetworkPolicyForDoctor("deny", 2, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if warn != "" {
		t.Fatalf("expected no warning, got %q", warn)
	}
}

func TestEvaluateNetworkPolicyForRunAcceptsAllowEntries(t *testing.T) {
	warn, err := evaluateNetworkPolicyForRun("deny", 2, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if warn != "" {
		t.Fatalf("expected no warning, got %q", warn)
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

func TestAllowlistSupportForConfigTreatsFileHandleModeAsSupported(t *testing.T) {
	t.Parallel()

	supported, detail, protectionMessage, err := allowlistSupportForConfig(backend.FirecrackerConfig{
		DarwinVZNetworkMode: darwinVZNetworkModeFileHandle,
	})
	if err != nil {
		t.Fatalf("allowlistSupportForConfig returned error: %v", err)
	}
	if !supported {
		t.Fatal("expected filehandle mode to support allowlists")
	}
	if detail != "" {
		t.Fatalf("expected empty detail, got %q", detail)
	}
	if got, want := protectionMessage, guestNetworkProtectedByFileHandleMessage; got != want {
		t.Fatalf("unexpected protection message: got %q want %q", got, want)
	}
}

func TestAllowlistSupportForConfigTreatsImplicitDefaultAsSupported(t *testing.T) {
	t.Parallel()

	supported, detail, protectionMessage, err := allowlistSupportForConfig(backend.FirecrackerConfig{})
	if err != nil {
		t.Fatalf("allowlistSupportForConfig returned error: %v", err)
	}
	if !supported {
		t.Fatal("expected implicit darwin-vz default to support allowlists")
	}
	if detail != "" {
		t.Fatalf("expected empty detail, got %q", detail)
	}
	if got, want := protectionMessage, guestNetworkProtectedByFileHandleMessage; got != want {
		t.Fatalf("unexpected protection message: got %q want %q", got, want)
	}
}

func TestEvaluateNetworkPolicyRejectsUnsupportedDefault(t *testing.T) {
	warn, err := evaluateNetworkPolicy("bogus", 0, false, false)
	if err == nil {
		t.Fatal("expected error for unsupported network default")
	}
	if warn != "" {
		t.Fatalf("expected empty warning when validation fails, got %q", warn)
	}
}
