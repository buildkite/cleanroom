//go:build darwin

package darwinvz

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/log"
)

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

func TestBuildRuntimeWarningsIncludesGuestNetworkWarningWhenProvided(t *testing.T) {
	warnings := buildRuntimeWarnings("", guestNetworkUnavailableWarning)
	if len(warnings) != 1 {
		t.Fatalf("expected one warning, got %d", len(warnings))
	}
	if warnings[0] != guestNetworkUnavailableWarning {
		t.Fatalf("unexpected warning: got %q want %q", warnings[0], guestNetworkUnavailableWarning)
	}
}

func TestBuildRuntimeWarningsIncludesPolicyWarningWhenPresent(t *testing.T) {
	warnings := buildRuntimeWarnings("  policy warn  ", guestNetworkUnavailableWarning)
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

func TestBuildRuntimeWarningsOmitsGuestNetworkWarningWhenEmpty(t *testing.T) {
	warnings := buildRuntimeWarnings("  policy warn  ", "")
	if len(warnings) != 1 {
		t.Fatalf("expected one warning, got %d", len(warnings))
	}
	if warnings[0] != "policy warn" {
		t.Fatalf("unexpected policy warning: got %q", warnings[0])
	}
}

func TestLogRunNoticeUsesCharmLogger(t *testing.T) {
	prev := log.Default()
	t.Cleanup(func() {
		log.SetDefault(prev)
	})

	var buf bytes.Buffer
	logger := log.NewWithOptions(&buf, log.Options{
		Level:     log.InfoLevel,
		Formatter: log.TextFormatter,
	})
	log.SetDefault(logger)

	logRunNotice("darwin-vz", "run-123", "using managed kernel asset my-kernel (cache hit)")

	out := buf.String()
	if !strings.Contains(out, "using managed kernel asset my-kernel (cache hit)") {
		t.Fatalf("expected notice text in logger output, got %q", out)
	}
	if !strings.Contains(out, "backend=darwin-vz") {
		t.Fatalf("expected backend field in logger output, got %q", out)
	}
	if !strings.Contains(out, "run_id=run-123") {
		t.Fatalf("expected run_id field in logger output, got %q", out)
	}
}
