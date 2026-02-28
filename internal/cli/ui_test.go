package cli

import (
	"regexp"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
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

	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("expected ANSI escapes in color output: %q", out)
	}
	if !strings.Contains(out, "cleanroom console") {
		t.Fatalf("missing title in header output: %q", out)
	}
	if !strings.Contains(out, "🧑‍🔬") {
		t.Fatalf("missing icon in header output: %q", out)
	}
	if !strings.Contains(out, "workspace: /tmp/repo") {
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

func stripANSI(value string) string {
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	return ansi.ReplaceAllString(value, "")
}
