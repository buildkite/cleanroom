package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBenchmarkDarwinVZMinimalRequiresKernelBeforeBuild(t *testing.T) {
	out, callLog, err := runBenchmarkDarwinVZMinimalExpectingEarlyFailure(t,
		"--iterations", "1",
	)
	requireBenchmarkDarwinVZMinimalFailure(t, out, err, callLog, "missing --kernel <path> or --build-kernel")
}

func TestBenchmarkDarwinVZMinimalRejectsRootFSKernelForInitrdBoot(t *testing.T) {
	out, callLog, err := runBenchmarkDarwinVZMinimalExpectingEarlyFailure(t,
		"--build-kernel",
		"--kernel-profile", "rootfs",
		"--iterations", "1",
	)
	requireBenchmarkDarwinVZMinimalFailure(t, out, err, callLog, "--kernel-profile rootfs does not match the selected boot medium; expected initrd")
}

func TestBenchmarkDarwinVZMinimalRejectsInitrdKernelForRootFSBoot(t *testing.T) {
	tmpDir := t.TempDir()
	rootFSPath := filepath.Join(tmpDir, "rootfs.ext4")
	if err := os.WriteFile(rootFSPath, []byte("placeholder"), 0o600); err != nil {
		t.Fatalf("write rootfs placeholder: %v", err)
	}

	out, callLog, err := runBenchmarkDarwinVZMinimalExpectingEarlyFailure(t,
		"--build-kernel",
		"--rootfs", rootFSPath,
		"--kernel-profile", "initrd",
		"--iterations", "1",
	)
	requireBenchmarkDarwinVZMinimalFailure(t, out, err, callLog, "--kernel-profile initrd does not match the selected boot medium; expected rootfs")
}

func runBenchmarkDarwinVZMinimalExpectingEarlyFailure(t *testing.T, args ...string) ([]byte, string, error) {
	t.Helper()

	tmpDir := t.TempDir()
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	callLog := filepath.Join(tmpDir, "xcrun-calls.log")
	writeLocalExecutable(t, binDir, "xcrun", `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_XCRUN_LOG"
exit 99
`)

	cmdArgs := append([]string{"benchmark-darwin-vz-minimal.sh"}, args...)
	cmdArgs = append(cmdArgs, "--output-dir", filepath.Join(tmpDir, "out"))
	cmd := exec.Command("bash", cmdArgs...)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_XCRUN_LOG="+callLog,
	)
	out, err := cmd.CombinedOutput()
	return out, callLog, err
}

func requireBenchmarkDarwinVZMinimalFailure(t *testing.T, out []byte, err error, callLog, wantMessage string) {
	t.Helper()

	if err == nil {
		t.Fatalf("expected benchmark-darwin-vz-minimal.sh to fail\n%s", out)
	}
	if !strings.Contains(string(out), wantMessage) {
		t.Fatalf("expected error %q, got:\n%s", wantMessage, out)
	}
	if _, err := os.Stat(callLog); err == nil {
		t.Fatalf("runner build should not start before kernel validation")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat xcrun call log: %v", err)
	}
}
