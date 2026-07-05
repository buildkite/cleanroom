package cli

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func makeStdoutCapture(t *testing.T) (*os.File, func() string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout-*.txt")
	if err != nil {
		t.Fatalf("create stdout capture: %v", err)
	}
	return f, func() string {
		if err := f.Sync(); err != nil {
			t.Fatalf("sync stdout capture: %v", err)
		}
		if _, err := f.Seek(0, 0); err != nil {
			t.Fatalf("seek stdout capture: %v", err)
		}
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatalf("read stdout capture: %v", err)
		}
		return string(b)
	}
}

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
