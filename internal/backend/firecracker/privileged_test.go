package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
)

func setupFakeSudo(t *testing.T, logPath string) {
	t.Helper()

	tmpDir := t.TempDir()
	fakeSudoPath := filepath.Join(tmpDir, "sudo")
	// Emulate `sudo -n <command ...>` for tests without requiring real sudo.
	fakeSudoScript := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$SUDO_LOG_PATH\"\nif [ \"$1\" = \"-n\" ]; then shift; fi\nexec \"$@\"\n"
	if err := os.WriteFile(fakeSudoPath, []byte(fakeSudoScript), 0o755); err != nil {
		t.Fatalf("write fake sudo script: %v", err)
	}
	t.Setenv("SUDO_LOG_PATH", logPath)
	t.Setenv("PATH", tmpDir+":"+os.Getenv("PATH"))
}

func TestRunRootCommandInvokesHelper(t *testing.T) {
	tmpDir := t.TempDir()
	sudoLogPath := filepath.Join(tmpDir, "sudo.log")
	logPath := filepath.Join(tmpDir, "helper.log")
	helperPath := filepath.Join(tmpDir, "cleanroom-root-helper")
	setupFakeSudo(t, sudoLogPath)

	helperScript := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$HELPER_LOG_PATH\"\n"
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
	t.Setenv("HELPER_LOG_PATH", logPath)

	cfg := backend.FirecrackerConfig{
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

	sudoLogBytes, err := os.ReadFile(sudoLogPath)
	if err != nil {
		t.Fatalf("read sudo log: %v", err)
	}
	if got := strings.TrimSpace(string(sudoLogBytes)); !strings.HasPrefix(got, "-n "+helperPath+" ") {
		t.Fatalf("expected helper invocation via sudo, got %q", got)
	}
}

func TestRunRootCommandBatchInvokesHelperPerCommand(t *testing.T) {
	tmpDir := t.TempDir()
	sudoLogPath := filepath.Join(tmpDir, "sudo.log")
	logPath := filepath.Join(tmpDir, "helper.log")
	helperPath := filepath.Join(tmpDir, "cleanroom-root-helper")
	setupFakeSudo(t, sudoLogPath)

	helperScript := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$HELPER_LOG_PATH\"\n"
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
	t.Setenv("HELPER_LOG_PATH", logPath)

	cfg := backend.FirecrackerConfig{
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

	sudoLogBytes, err := os.ReadFile(sudoLogPath)
	if err != nil {
		t.Fatalf("read sudo log: %v", err)
	}
	sudoLines := strings.Split(strings.TrimSpace(string(sudoLogBytes)), "\n")
	if len(sudoLines) != 2 {
		t.Fatalf("expected two sudo invocations, got %d (%q)", len(sudoLines), string(sudoLogBytes))
	}
}

func TestRunRootCommandOutputInvokesHelper(t *testing.T) {
	tmpDir := t.TempDir()
	sudoLogPath := filepath.Join(tmpDir, "sudo.log")
	logPath := filepath.Join(tmpDir, "helper.log")
	helperPath := filepath.Join(tmpDir, "cleanroom-root-helper")
	setupFakeSudo(t, sudoLogPath)

	helperScript := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$HELPER_LOG_PATH\"\nprintf 'firecracker-network\\nfirecracker-rootfs\\n'\n"
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
	t.Setenv("HELPER_LOG_PATH", logPath)

	cfg := backend.FirecrackerConfig{
		PrivilegedHelperPath: helperPath,
	}

	out, err := runRootCommandOutput(context.Background(), cfg, "capabilities")
	if err != nil {
		t.Fatalf("runRootCommandOutput: %v", err)
	}
	if got, want := strings.TrimSpace(string(out)), "firecracker-network\nfirecracker-rootfs"; got != want {
		t.Fatalf("unexpected helper output: got %q want %q", got, want)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read helper log: %v", err)
	}
	if got := strings.TrimSpace(string(logBytes)); got != "capabilities" {
		t.Fatalf("unexpected helper invocation: got %q want %q", got, "capabilities")
	}
}

func TestHelperCapabilitiesReturnsProbeError(t *testing.T) {
	tmpDir := t.TempDir()
	sudoLogPath := filepath.Join(tmpDir, "sudo.log")
	helperPath := filepath.Join(tmpDir, "cleanroom-root-helper")
	setupFakeSudo(t, sudoLogPath)

	helperScript := "#!/bin/sh\nset -eu\necho \"cleanroom-root-helper: unsupported command 'capabilities'\" >&2\nexit 2\n"
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}

	cfg := backend.FirecrackerConfig{
		PrivilegedHelperPath: helperPath,
	}

	_, err := helperCapabilities(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected helperCapabilities to fail for unsupported probe")
	}
	if !strings.Contains(err.Error(), "unsupported command 'capabilities'") {
		t.Fatalf("unexpected helperCapabilities error: %v", err)
	}
}

func TestHelperVersionReturnsProbeError(t *testing.T) {
	tmpDir := t.TempDir()
	sudoLogPath := filepath.Join(tmpDir, "sudo.log")
	helperPath := filepath.Join(tmpDir, "cleanroom-root-helper")
	setupFakeSudo(t, sudoLogPath)

	helperScript := "#!/bin/sh\nset -eu\necho \"cleanroom-root-helper: unsupported command 'version'\" >&2\nexit 2\n"
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}

	cfg := backend.FirecrackerConfig{
		PrivilegedHelperPath: helperPath,
	}

	_, err := helperVersion(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected helperVersion to fail for unsupported probe")
	}
	if !strings.Contains(err.Error(), "unsupported command 'version'") {
		t.Fatalf("unexpected helperVersion error: %v", err)
	}
}

func TestResolvePrivilegedHelperPathDefaultsToInstalledPath(t *testing.T) {
	t.Parallel()

	if got, want := resolvePrivilegedHelperPath(backend.FirecrackerConfig{}), defaultPrivilegedHelperPath; got != want {
		t.Fatalf("unexpected helper path: got %q want %q", got, want)
	}
}

func TestHelperRequiredCapabilitiesIncludesZFSWhenConfigured(t *testing.T) {
	t.Parallel()

	got := helperRequiredCapabilities(backend.FirecrackerConfig{})
	want := []string{
		helperCapabilityFirecrackerNetwork,
		helperCapabilityFirecrackerRootFS,
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected default helper capabilities: got %v want %v", got, want)
	}

	got = helperRequiredCapabilities(backend.FirecrackerConfig{
		Snapshots: backend.SnapshotConfig{Driver: "zfs"},
	})
	want = []string{
		helperCapabilityFirecrackerNetwork,
		helperCapabilityFirecrackerRootFS,
		helperCapabilityFirecrackerZFS,
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected zfs helper capabilities: got %v want %v", got, want)
	}
}
