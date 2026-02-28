//go:build darwin

package darwinvz

import "testing"

func TestDarwinVZResultMessageEmptyWithoutGuestError(t *testing.T) {
	if got := darwinVZResultMessage(""); got != "" {
		t.Fatalf("expected empty message, got %q", got)
	}
	if got := darwinVZResultMessage("   "); got != "" {
		t.Fatalf("expected empty message for whitespace guest error, got %q", got)
	}
}

func TestDarwinVZResultMessageIncludesGuestError(t *testing.T) {
	got := darwinVZResultMessage(" runtime failed ")
	want := "runtime failed"
	if got != want {
		t.Fatalf("unexpected message: got %q want %q", got, want)
	}
}

func TestBuildRuntimeWarningsAlwaysIncludesGuestNetworkWarning(t *testing.T) {
	warnings := buildRuntimeWarnings("")
	if len(warnings) != 1 {
		t.Fatalf("expected one warning, got %d", len(warnings))
	}
	if warnings[0] != guestNetworkUnavailableWarning {
		t.Fatalf("unexpected warning: got %q want %q", warnings[0], guestNetworkUnavailableWarning)
	}
}

func TestBuildRuntimeWarningsIncludesPolicyWarningWhenPresent(t *testing.T) {
	warnings := buildRuntimeWarnings("  policy warn  ")
	if len(warnings) != 2 {
		t.Fatalf("expected two warnings, got %d", len(warnings))
	}
	if warnings[0] != "policy warn" {
		t.Fatalf("unexpected policy warning: got %q", warnings[0])
	}
	if warnings[1] != guestNetworkUnavailableWarning {
		t.Fatalf("unexpected guest networking warning: got %q", warnings[1])
	}
}
