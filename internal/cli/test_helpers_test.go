package cli

import (
	"regexp"
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

func stripANSI(value string) string {
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	return ansi.ReplaceAllString(value, "")
}
