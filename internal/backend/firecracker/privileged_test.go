package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
)

func TestRunRootCommandHelperModeInvokesHelper(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "helper.log")
	helperPath := filepath.Join(tmpDir, "cleanroom-root-helper")

	helperScript := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$HELPER_LOG_PATH\"\n"
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
	t.Setenv("HELPER_LOG_PATH", logPath)

	cfg := backend.FirecrackerConfig{
		PrivilegedMode:       privilegedModeHelper,
		PrivilegedHelperPath: helperPath,
	}

	if err := runRootCommand(context.Background(), cfg, "ip", "link", "show"); err != nil {
		t.Fatalf("runRootCommand: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read helper log: %v", err)
	}
	if got := strings.TrimSpace(string(logBytes)); got != "ip link show" {
		t.Fatalf("unexpected helper invocation: got %q want %q", got, "ip link show")
	}
}

func TestRunRootCommandBatchHelperModeInvokesHelperPerCommand(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "helper.log")
	helperPath := filepath.Join(tmpDir, "cleanroom-root-helper")

	helperScript := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$HELPER_LOG_PATH\"\n"
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
	t.Setenv("HELPER_LOG_PATH", logPath)

	cfg := backend.FirecrackerConfig{
		PrivilegedMode:       privilegedModeHelper,
		PrivilegedHelperPath: helperPath,
	}

	commands := [][]string{{"ip", "link", "del", "tap0"}, {"iptables", "-D", "FORWARD", "-j", "DROP"}}
	if err := runRootCommandBatch(context.Background(), cfg, commands); err != nil {
		t.Fatalf("runRootCommandBatch: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read helper log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(logBytes)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two helper invocations, got %d (%q)", len(lines), string(logBytes))
	}
	if got, want := lines[0], "ip link del tap0"; got != want {
		t.Fatalf("unexpected first helper invocation: got %q want %q", got, want)
	}
	if got, want := lines[1], "iptables -D FORWARD -j DROP"; got != want {
		t.Fatalf("unexpected second helper invocation: got %q want %q", got, want)
	}
}

func TestResolvePrivilegedExecutionDefaultsToSudo(t *testing.T) {
	t.Parallel()

	mode, helperPath := resolvePrivilegedExecution(backend.FirecrackerConfig{})
	if got, want := mode, privilegedModeSudo; got != want {
		t.Fatalf("unexpected mode: got %q want %q", got, want)
	}
	if got, want := helperPath, defaultPrivilegedHelperPath; got != want {
		t.Fatalf("unexpected helper path: got %q want %q", got, want)
	}
}
