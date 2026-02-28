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
