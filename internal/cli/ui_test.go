package cli

import (
	"strings"
	"testing"
)

func TestRenderKeyValueLinePlainAndColor(t *testing.T) {
	palette := defaultTerminalPalette()

	plain := renderKeyValueLine("  ", "policy hash", "abc123", false, palette)
	if plain != "  policy hash: abc123" {
		t.Fatalf("plain line = %q", plain)
	}

	colored := renderKeyValueLine("", "policy hash", "abc123", true, palette)
	if !strings.Contains(colored, "\x1b[") {
		t.Fatalf("expected ANSI in colored line, got %q", colored)
	}
	if stripANSI(colored) != "policy hash: abc123" {
		t.Fatalf("stripped colored line = %q", stripANSI(colored))
	}
}

func TestRenderStatusValueLine(t *testing.T) {
	palette := defaultTerminalPalette()
	line := renderStatusValueLine("policy valid", "cleanroom.yaml", palette.info, false)
	if line != "policy valid: cleanroom.yaml" {
		t.Fatalf("status line = %q", line)
	}
}

func TestShouldUseANSIRespectsEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if shouldUseANSI(nil) {
		t.Fatal("NO_COLOR should disable ANSI")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "1")
	if !shouldUseANSI(nil) {
		t.Fatal("CLICOLOR_FORCE should force ANSI")
	}
}
