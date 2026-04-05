package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
)

func writeExecutable(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", name, err)
	}
	return path
}

func doctorCheck(report *backend.DoctorReport, name string) backend.DoctorCheck {
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	return backend.DoctorCheck{}
}

func doctorHasCheck(report *backend.DoctorReport, name string) bool {
	for _, check := range report.Checks {
		if check.Name == name {
			return true
		}
	}
	return false
}

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

	helperScript := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$HELPER_LOG_PATH\"\nprintf 'firecracker-network\\n'\n"
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
	if got, want := strings.TrimSpace(string(out)), "firecracker-network"; got != want {
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

func TestRunRootCommandOutputInvokesHelperForZFSProbe(t *testing.T) {
	tmpDir := t.TempDir()
	sudoLogPath := filepath.Join(tmpDir, "sudo.log")
	logPath := filepath.Join(tmpDir, "helper.log")
	helperPath := filepath.Join(tmpDir, "cleanroom-root-helper")
	setupFakeSudo(t, sudoLogPath)

	helperScript := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$HELPER_LOG_PATH\"\nif [ \"$1\" = \"zfs\" ] && [ \"$2\" = \"list\" ] && [ \"$3\" = \"-H\" ] && [ \"$4\" = \"-o\" ] && [ \"$5\" = \"name\" ]; then printf '%s\\n' \"$6\"; fi\n"
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
	t.Setenv("HELPER_LOG_PATH", logPath)

	cfg := backend.FirecrackerConfig{
		PrivilegedHelperPath: helperPath,
	}

	out, err := runRootCommandOutput(context.Background(), cfg, "zfs", "list", "-H", "-o", "name", "tank/cleanroom")
	if err != nil {
		t.Fatalf("runRootCommandOutput: %v", err)
	}
	if got, want := strings.TrimSpace(string(out)), "tank/cleanroom"; got != want {
		t.Fatalf("unexpected helper output: got %q want %q", got, want)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read helper log: %v", err)
	}
	if got := strings.TrimSpace(string(logBytes)); got != "zfs list -H -o name tank/cleanroom" {
		t.Fatalf("unexpected helper invocation: got %q", got)
	}
}

func TestDoctorReportsZFSChecks(t *testing.T) {
	tmpDir := t.TempDir()
	sudoLogPath := filepath.Join(tmpDir, "sudo.log")
	setupFakeSudo(t, sudoLogPath)
	logPath := filepath.Join(tmpDir, "helper.log")
	t.Setenv("HELPER_LOG_PATH", logPath)

	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin dir: %v", err)
	}

	writeExecutable(t, binDir, "firecracker", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, binDir, "cleanroom-guest-agent", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, binDir, "mkfs.ext4", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, binDir, "iptables", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, binDir, "sysctl", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, binDir, "true", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, binDir, "ip", "#!/bin/sh\nif [ \"$1\" = \"link\" ] && [ \"$2\" = \"show\" ]; then exit 0; fi\nexit 0\n")
	writeExecutable(t, binDir, "zfs", "#!/bin/sh\nif [ \"$1\" = \"list\" ] && [ \"$2\" = \"-H\" ] && [ \"$3\" = \"-d\" ] && [ \"$4\" = \"0\" ] && [ \"$5\" = \"-o\" ] && [ \"$6\" = \"name\" ]; then printf '%s\\n' \"$7\"; exit 0; fi\nif [ \"$1\" = \"list\" ] && [ \"$2\" = \"-H\" ] && [ \"$3\" = \"-o\" ] && [ \"$4\" = \"name\" ]; then printf '%s\\n%s/child\\n' \"$5\" \"$5\"; exit 0; fi\nexit 0\n")
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	helperPath := writeExecutable(t, tmpDir, "cleanroom-root-helper", "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$HELPER_LOG_PATH\"\ncase \"$1\" in\n  version)\n    printf 'test-helper\\n'\n    ;;\n  capabilities)\n    printf 'firecracker-network\\nfirecracker-trusted-dns\\nfirecracker-rootfs\\nfirecracker-zfs\\n'\n    ;;\n  true)\n    exec \"$@\"\n    ;;\n  zfs)\n    if [ \"$2\" = \"list\" ] && [ \"$3\" = \"-H\" ] && [ \"$4\" = \"-d\" ] && [ \"$5\" = \"0\" ] && [ \"$6\" = \"-o\" ] && [ \"$7\" = \"name\" ]; then\n      printf '%s\\n' \"$8\"\n      exit 0\n    fi\n    echo 'unexpected helper zfs args' >&2\n    exit 2\n    ;;\n  *)\n    exec \"$@\"\n    ;;\n esac\n")

	kernelPath := filepath.Join(tmpDir, "vmlinux")
	if err := os.WriteFile(kernelPath, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("write kernel image: %v", err)
	}

	report, err := (&Adapter{}).Doctor(context.Background(), backend.DoctorRequest{
		FirecrackerConfig: backend.FirecrackerConfig{
			BinaryPath:           "firecracker",
			KernelImagePath:      kernelPath,
			PrivilegedHelperPath: helperPath,
			Snapshots: backend.SnapshotConfig{
				Driver:     "zfs",
				ZFSDataset: "tank/cleanroom",
			},
		},
	})
	if err != nil {
		t.Fatalf("Doctor returned error: %v", err)
	}

	if got := doctorCheck(report, "snapshot_driver"); got.Status != "pass" {
		t.Fatalf("unexpected snapshot_driver check: %+v", got)
	}
	if got := doctorCheck(report, "snapshot_zfs_dataset"); got.Status != "pass" {
		t.Fatalf("unexpected snapshot_zfs_dataset check: %+v", got)
	}
	if got := doctorCheck(report, "snapshot_zfs_binary"); got.Status != "pass" {
		t.Fatalf("unexpected snapshot_zfs_binary check: %+v", got)
	}
	if got := doctorCheck(report, "snapshot_zfs_dataset_access"); got.Status != "pass" {
		t.Fatalf("unexpected snapshot_zfs_dataset_access check: %+v", got)
	}
	if doctorHasCheck(report, "network_cmd_ipset") {
		t.Fatalf("doctor unexpectedly requires ipset: %+v", doctorCheck(report, "network_cmd_ipset"))
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read helper log: %v", err)
	}
	if !strings.Contains(string(logBytes), "zfs list -H -d 0 -o name tank/cleanroom") {
		t.Fatalf("expected helper-mediated zfs probe, got log %q", string(logBytes))
	}
}

func TestDeleteSnapshotDerivesZFSDatasetFromStorageRef(t *testing.T) {
	tmpDir := t.TempDir()
	sudoLogPath := filepath.Join(tmpDir, "sudo.log")
	setupFakeSudo(t, sudoLogPath)

	logPath := filepath.Join(tmpDir, "helper.log")
	helperPath := writeExecutable(t, tmpDir, "cleanroom-root-helper", "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$HELPER_LOG_PATH\"\nexit 0\n")
	t.Setenv("HELPER_LOG_PATH", logPath)

	err := (&Adapter{}).DeleteSnapshot(context.Background(), backend.DeleteSnapshotRequest{
		StorageRef: "tank/cleanroom/snapshots/snap-test@seed",
		FirecrackerConfig: backend.FirecrackerConfig{
			PrivilegedHelperPath: helperPath,
			Snapshots: backend.SnapshotConfig{
				Enabled: true,
				Driver:  "zfs",
			},
		},
	})
	if err != nil {
		t.Fatalf("DeleteSnapshot returned error: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read helper log: %v", err)
	}
	if got, want := strings.TrimSpace(string(logBytes)), "zfs destroy -r tank/cleanroom/snapshots/snap-test"; got != want {
		t.Fatalf("unexpected helper invocation: got %q want %q", got, want)
	}
}

func TestDeleteSnapshotInfersZFSDriverFromStorageRef(t *testing.T) {
	tmpDir := t.TempDir()
	sudoLogPath := filepath.Join(tmpDir, "sudo.log")
	setupFakeSudo(t, sudoLogPath)

	logPath := filepath.Join(tmpDir, "helper.log")
	helperPath := writeExecutable(t, tmpDir, "cleanroom-root-helper", "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$HELPER_LOG_PATH\"\nexit 0\n")
	t.Setenv("HELPER_LOG_PATH", logPath)

	err := (&Adapter{}).DeleteSnapshot(context.Background(), backend.DeleteSnapshotRequest{
		StorageRef: "tank/cleanroom/snapshots/snap-test@seed",
		FirecrackerConfig: backend.FirecrackerConfig{
			PrivilegedHelperPath: helperPath,
			Snapshots: backend.SnapshotConfig{
				Enabled: true,
			},
		},
	})
	if err != nil {
		t.Fatalf("DeleteSnapshot returned error: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read helper log: %v", err)
	}
	if got, want := strings.TrimSpace(string(logBytes)), "zfs destroy -r tank/cleanroom/snapshots/snap-test"; got != want {
		t.Fatalf("unexpected helper invocation: got %q want %q", got, want)
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
		helperCapabilityFirecrackerTrustedDNS,
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected default helper capabilities: got %v want %v", got, want)
	}

	got = helperRequiredCapabilities(backend.FirecrackerConfig{
		Snapshots: backend.SnapshotConfig{Driver: "zfs"},
	})
	want = []string{
		helperCapabilityFirecrackerNetwork,
		helperCapabilityFirecrackerTrustedDNS,
		helperCapabilityFirecrackerZFS,
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected zfs helper capabilities: got %v want %v", got, want)
	}
}
