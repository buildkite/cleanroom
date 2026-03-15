package cli

import (
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/charmbracelet/log"
)

func TestRenderStartupHeaderPlain(t *testing.T) {
	out := renderStartupHeader(startupHeader{
		Title: "cleanroom exec",
		Fields: []startupField{
			{Key: "workspace", Value: "/tmp/repo"},
			{Key: "backend", Value: "firecracker"},
		},
	}, false)

	want := "\n🧑‍🔬 cleanroom exec\n   workspace: /tmp/repo\n   backend: firecracker\n\n"
	if out != want {
		t.Fatalf("unexpected header output:\n--- got ---\n%s--- want ---\n%s", out, want)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("plain output should not contain ANSI escapes: %q", out)
	}
}

func TestRenderStartupHeaderColor(t *testing.T) {
	out := renderStartupHeader(startupHeader{
		Title: "cleanroom console",
		Fields: []startupField{
			{Key: "workspace", Value: "/tmp/repo"},
		},
	}, true)
	plain := stripANSI(out)

	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("expected ANSI escapes in color output: %q", out)
	}
	if !strings.Contains(plain, "cleanroom console") {
		t.Fatalf("missing title in header output: %q", out)
	}
	if !strings.Contains(plain, "🧑‍🔬") {
		t.Fatalf("missing icon in header output: %q", out)
	}
	if !strings.Contains(plain, "workspace: /tmp/repo") {
		t.Fatalf("missing field in header output: %q", out)
	}
	if !strings.HasPrefix(out, "\n") {
		t.Fatalf("expected leading blank line in header output: %q", out)
	}
	if !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("expected trailing blank line in header output: %q", out)
	}
}

func TestRenderStartupHeaderSkipsEmptyFields(t *testing.T) {
	out := renderStartupHeader(startupHeader{
		Title: "cleanroom exec",
		Fields: []startupField{
			{Key: "workspace", Value: "/tmp/repo"},
			{Key: "backend", Value: ""},
			{Key: "", Value: "ignored"},
		},
	}, false)

	if strings.Contains(out, "backend:") {
		t.Fatalf("expected empty backend field to be omitted: %q", out)
	}
	if strings.Contains(out, "ignored") {
		t.Fatalf("expected field without key to be omitted: %q", out)
	}
}

func TestRenderDoctorReportPlain(t *testing.T) {
	out := renderDoctorReport("firecracker", []backend.DoctorCheck{
		{Name: "runtime_config", Status: "pass", Message: "using /tmp/config.yaml"},
		{Name: "network_guest_interface", Status: "warn", Message: "unsupported"},
	}, false)

	if !strings.Contains(out, "doctor report (firecracker)") {
		t.Fatalf("missing doctor title: %q", out)
	}
	if !strings.Contains(out, "✓ [pass] runtime_config: using /tmp/config.yaml") {
		t.Fatalf("missing pass line: %q", out)
	}
	if !strings.Contains(out, "! [warn] network_guest_interface: unsupported") {
		t.Fatalf("missing warn line: %q", out)
	}
	if !strings.Contains(out, "summary: 1 pass, 1 warn, 0 fail") {
		t.Fatalf("missing summary line: %q", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("plain output should not contain ANSI escapes: %q", out)
	}
}

func TestRenderDoctorReportColor(t *testing.T) {
	out := renderDoctorReport("darwin-vz", []backend.DoctorCheck{
		{Name: "backend_doctor", Status: "fail", Message: "missing helper"},
	}, true)
	plain := stripANSI(out)

	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("expected ANSI escapes in color output: %q", out)
	}
	if !strings.Contains(plain, "doctor report (darwin-vz)") {
		t.Fatalf("missing doctor title: %q", out)
	}
	if !strings.Contains(plain, "✗ [fail] backend_doctor: missing helper") {
		t.Fatalf("missing fail line: %q", out)
	}
	if !strings.Contains(plain, "summary: 0 pass, 0 warn, 1 fail") {
		t.Fatalf("missing summary line: %q", out)
	}
}

func TestRenderDoctorReportColorUsesLoggerPalette(t *testing.T) {
	out := renderDoctorReport("darwin-vz", []backend.DoctorCheck{
		{Name: "runtime_config", Status: "pass", Message: "using /tmp/config.yaml"},
		{Name: "network_guest_interface", Status: "warn", Message: "unsupported"},
		{Name: "backend_doctor", Status: "fail", Message: "missing helper"},
	}, true)
	palette := defaultTerminalPalette()

	if !strings.Contains(out, palette.info.wrap("✓ [pass]", true)) {
		t.Fatalf("expected pass status to use info palette, got: %q", out)
	}
	if !strings.Contains(out, palette.warn.wrap("! [warn]", true)) {
		t.Fatalf("expected warn status to use warn palette, got: %q", out)
	}
	if !strings.Contains(out, palette.error.wrap("✗ [fail]", true)) {
		t.Fatalf("expected fail status to use error palette, got: %q", out)
	}
}

func TestRenderDaemonStatusReportPlain(t *testing.T) {
	out := renderDaemonStatusReport(daemonStatusReport{
		Manager:   "launchd",
		Service:   "com.buildkite.cleanroom",
		Installed: false,
		Active:    false,
		Fields: []startupField{
			{Key: "install", Value: "missing"},
			{Key: "runtime", Value: "inactive"},
			{Key: "domain", Value: "gui/501"},
			{Key: "path", Value: "/Users/alice/Library/LaunchAgents/com.buildkite.cleanroom.plist"},
		},
	}, false)

	if !strings.Contains(out, "daemon status (launchd)") {
		t.Fatalf("missing status title: %q", out)
	}
	if !strings.Contains(out, "! [not installed] com.buildkite.cleanroom") {
		t.Fatalf("missing summary line: %q", out)
	}
	if !strings.Contains(out, "  install: missing") {
		t.Fatalf("missing install line: %q", out)
	}
	if !strings.Contains(out, "  runtime: inactive") {
		t.Fatalf("missing runtime line: %q", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("plain output should not contain ANSI escapes: %q", out)
	}
}

func TestRenderDaemonStatusReportColor(t *testing.T) {
	out := renderDaemonStatusReport(daemonStatusReport{
		Manager:   "systemd",
		Service:   "cleanroom.service",
		Installed: true,
		Active:    true,
		Fields: []startupField{
			{Key: "install", Value: "installed"},
			{Key: "runtime", Value: "active"},
			{Key: "enabled", Value: "enabled"},
		},
	}, true)
	plain := stripANSI(out)

	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("expected ANSI escapes in color output: %q", out)
	}
	if !strings.Contains(plain, "daemon status (systemd)") {
		t.Fatalf("missing status title: %q", out)
	}
	if !strings.Contains(plain, "✓ [running] cleanroom.service") {
		t.Fatalf("missing summary line: %q", out)
	}
	if !strings.Contains(plain, "  enabled: enabled") {
		t.Fatalf("missing enabled line: %q", out)
	}
}

func TestRenderDaemonStatusReportColorUsesLoggerPalette(t *testing.T) {
	out := renderDaemonStatusReport(daemonStatusReport{
		Manager:   "launchd",
		Service:   "com.buildkite.cleanroom",
		Installed: false,
		Active:    false,
		Fields: []startupField{
			{Key: "install", Value: "missing"},
			{Key: "runtime", Value: "inactive"},
		},
	}, true)
	palette := defaultTerminalPalette()
	expectedField := "  " + palette.key.wrap("install", true) + palette.separator.wrap(":", true) + " " + palette.value.wrap("missing", true)

	if !strings.Contains(out, palette.warn.wrap("! [not installed]", true)) {
		t.Fatalf("expected daemon summary to use warn palette, got: %q", out)
	}
	if !strings.Contains(out, expectedField) {
		t.Fatalf("expected daemon fields to use shared key/value palette, got: %q", out)
	}
}

func TestDefaultTerminalPaletteUsesLoggerDefaults(t *testing.T) {
	styles := log.DefaultStyles()
	palette := defaultTerminalPalette()

	if got, want := palette.text, terminalStyleFromLipgloss(styles.Message); got != want {
		t.Fatalf("message style mismatch: got %+v want %+v", got, want)
	}
	if got, want := palette.value, terminalStyleFromLipgloss(styles.Value); got != want {
		t.Fatalf("value style mismatch: got %+v want %+v", got, want)
	}
	if got, want := palette.separator, terminalStyleFromLipgloss(styles.Separator); got != want {
		t.Fatalf("separator style mismatch: got %+v want %+v", got, want)
	}
	if got, want := palette.key, terminalStyleFromLipgloss(styles.Key); got != want {
		t.Fatalf("key style mismatch: got %+v want %+v", got, want)
	}
	if got, want := palette.info, terminalStyleFromLipgloss(styles.Levels[log.InfoLevel]); got != want {
		t.Fatalf("info style mismatch: got %+v want %+v", got, want)
	}
	if got, want := palette.warn, terminalStyleFromLipgloss(styles.Levels[log.WarnLevel]); got != want {
		t.Fatalf("warn style mismatch: got %+v want %+v", got, want)
	}
	if got, want := palette.error, terminalStyleFromLipgloss(styles.Levels[log.ErrorLevel]); got != want {
		t.Fatalf("error style mismatch: got %+v want %+v", got, want)
	}
}

func TestRenderDaemonStatusReportShowsRunningWhenActiveWithoutInstall(t *testing.T) {
	out := renderDaemonStatusReport(daemonStatusReport{
		Manager:   "launchd",
		Service:   "com.buildkite.cleanroom",
		Installed: false,
		Active:    true,
		Fields: []startupField{
			{Key: "install", Value: "missing"},
			{Key: "runtime", Value: "active"},
		},
	}, false)

	if !strings.Contains(out, "✓ [running] com.buildkite.cleanroom") {
		t.Fatalf("expected running summary when runtime is active, got: %q", out)
	}
	if !strings.Contains(out, "  install: missing") {
		t.Fatalf("missing install line: %q", out)
	}
}

func TestRenderSummaryBlockColorUsesSharedPalette(t *testing.T) {
	out := renderSummaryBlock(summaryBlock{
		Title:      "pulled image",
		TitleStyle: defaultTerminalPalette().info,
		Fields: []startupField{
			{Key: "ref", Value: "ghcr.io/buildkite/cleanroom-base/alpine@sha256:1234"},
			{Key: "digest", Value: "sha256:1234"},
		},
	}, true)
	plain := stripANSI(out)
	palette := defaultTerminalPalette()

	if !strings.Contains(out, palette.info.wrap("pulled image", true)) {
		t.Fatalf("expected summary title to use info palette, got: %q", out)
	}
	if !strings.Contains(out, "ref") || !strings.Contains(out, palette.separator.wrap("=", true)) {
		t.Fatalf("expected assignment fields to be rendered with shared separator styling, got: %q", out)
	}
	if !strings.Contains(plain, "pulled image\nref=ghcr.io/buildkite/cleanroom-base/alpine@sha256:1234\ndigest=sha256:1234\n") {
		t.Fatalf("unexpected plain summary output: %q", plain)
	}
}

func TestRenderNoticeLineColorUsesSharedPalette(t *testing.T) {
	out := renderNoticeLine("warning", "repository has local modifications", defaultTerminalPalette().warn, true)
	plain := stripANSI(out)

	if !strings.Contains(out, defaultTerminalPalette().warn.wrap("warning", true)) {
		t.Fatalf("expected notice prefix to use warn palette, got: %q", out)
	}
	if plain != "warning: repository has local modifications\n" {
		t.Fatalf("unexpected plain notice output: %q", plain)
	}
}
