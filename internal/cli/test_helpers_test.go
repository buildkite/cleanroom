package cli

import (
	"strings"
	"testing"
)

func assertContainsAll(t *testing.T, got string, want ...string) {
	t.Helper()

	for _, expected := range want {
		if !strings.Contains(got, expected) {
			t.Fatalf("expected output to contain %q, got %q", expected, got)
		}
	}
}
