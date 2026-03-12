package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestShouldInstallGatewayFirewall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		goos string
		want bool
	}{
		{name: "linux", goos: "linux", want: true},
		{name: "linux case-insensitive", goos: "LiNuX", want: true},
		{name: "darwin", goos: "darwin", want: false},
		{name: "windows", goos: "windows", want: false},
		{name: "whitespace", goos: "  linux  ", want: true},
		{name: "empty", goos: "", want: false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldInstallGatewayFirewall(tc.goos); got != tc.want {
				t.Fatalf("shouldInstallGatewayFirewall(%q) = %v, want %v", tc.goos, got, tc.want)
			}
		})
	}
}

func TestServeInstallRequiresRoot(t *testing.T) {
	prevEUID := serveInstallEUID
	prevGOOS := serveInstallGOOS
	serveInstallEUID = func() int { return 1000 }
	serveInstallGOOS = "linux"
	t.Cleanup(func() {
		serveInstallEUID = prevEUID
		serveInstallGOOS = prevGOOS
	})

	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "install"}
	err := cmd.Run(&runtimeContext{CWD: t.TempDir(), Stdout: stdout})
	if err == nil {
		t.Fatal("expected root requirement error")
	}
	if !strings.Contains(err.Error(), "requires root") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "sudo cleanroom serve install") {
		t.Fatalf("expected sudo guidance, got: %v", err)
	}
}

func TestServeRunRejectsScopeFlags(t *testing.T) {
	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{User: true}
	err := cmd.Run(&runtimeContext{CWD: t.TempDir(), Stdout: stdout})
	if err == nil {
		t.Fatal("expected scope-flag validation error")
	}
	if !strings.Contains(err.Error(), "only supported with") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestServeInstallRefusesOverwriteWithoutForce(t *testing.T) {
	tmpDir := t.TempDir()
	unitPath := filepath.Join(tmpDir, "cleanroom.service")
	original := []byte("existing-unit")
	if err := os.WriteFile(unitPath, original, 0o644); err != nil {
		t.Fatalf("write existing unit: %v", err)
	}

	prevEUID := serveInstallEUID
	prevGOOS := serveInstallGOOS
	prevSystemdPath := serveInstallSystemdUnitPath
	prevExecutable := serveInstallExecutablePath
	prevRunCommand := serveInstallRunCommand
	serveInstallEUID = func() int { return 0 }
	serveInstallGOOS = "linux"
	serveInstallSystemdUnitPath = unitPath
	serveInstallExecutablePath = func() (string, error) { return "/usr/local/bin/cleanroom", nil }
	runCalls := 0
	serveInstallRunCommand = func(name string, args ...string) error {
		runCalls++
		return nil
	}
	t.Cleanup(func() {
		serveInstallEUID = prevEUID
		serveInstallGOOS = prevGOOS
		serveInstallSystemdUnitPath = prevSystemdPath
		serveInstallExecutablePath = prevExecutable
		serveInstallRunCommand = prevRunCommand
	})

	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "install"}
	err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout})
	if err == nil {
		t.Fatal("expected overwrite refusal")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("expected force guidance, got: %v", err)
	}
	if runCalls != 0 {
		t.Fatalf("expected no service manager commands, got %d", runCalls)
	}

	raw, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unit file: %v", err)
	}
	if got, want := string(raw), string(original); got != want {
		t.Fatalf("unit content changed unexpectedly: got %q want %q", got, want)
	}
}

func TestServeInstallForceOverwritesAndEnablesService(t *testing.T) {
	tmpDir := t.TempDir()
	unitPath := filepath.Join(tmpDir, "cleanroom.service")
	if err := os.WriteFile(unitPath, []byte("existing-unit"), 0o644); err != nil {
		t.Fatalf("write existing unit: %v", err)
	}

	prevEUID := serveInstallEUID
	prevGOOS := serveInstallGOOS
	prevSystemdPath := serveInstallSystemdUnitPath
	prevExecutable := serveInstallExecutablePath
	prevRunCommand := serveInstallRunCommand
	serveInstallEUID = func() int { return 0 }
	serveInstallGOOS = "linux"
	serveInstallSystemdUnitPath = unitPath
	serveInstallExecutablePath = func() (string, error) { return "/usr/local/bin/cleanroom", nil }
	var calls [][]string
	serveInstallRunCommand = func(name string, args ...string) error {
		call := append([]string{name}, args...)
		calls = append(calls, call)
		return nil
	}
	t.Cleanup(func() {
		serveInstallEUID = prevEUID
		serveInstallGOOS = prevGOOS
		serveInstallSystemdUnitPath = prevSystemdPath
		serveInstallExecutablePath = prevExecutable
		serveInstallRunCommand = prevRunCommand
	})

	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "install", Force: true}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout}); err != nil {
		t.Fatalf("ServeCommand.Run returned error: %v", err)
	}

	raw, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read generated unit: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "ExecStart=/usr/local/bin/cleanroom serve") {
		t.Fatalf("expected serve exec start, got:\n%s", content)
	}
	if !strings.Contains(content, "--listen unix:///var/run/cleanroom/cleanroom.sock") {
		t.Fatalf("expected explicit default --listen in unit, got:\n%s", content)
	}
	if strings.Contains(content, "serve install") {
		t.Fatalf("unit should run server mode, not install mode:\n%s", content)
	}

	wantCalls := [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "--now", "cleanroom.service"},
		{"systemctl", "restart", "cleanroom.service"},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("unexpected systemctl commands: got %v want %v", calls, wantCalls)
	}
}

func TestServeInstallUnsupportedOSCheckedBeforeRoot(t *testing.T) {
	prevEUID := serveInstallEUID
	prevGOOS := serveInstallGOOS
	serveInstallEUID = func() int { return 1000 }
	serveInstallGOOS = "windows"
	t.Cleanup(func() {
		serveInstallEUID = prevEUID
		serveInstallGOOS = prevGOOS
	})

	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "install"}
	err := cmd.Run(&runtimeContext{CWD: t.TempDir(), Stdout: stdout})
	if err == nil {
		t.Fatal("expected unsupported OS error")
	}
	if !strings.Contains(err.Error(), "unsupported on windows") {
		t.Fatalf("expected unsupported OS message, got: %v", err)
	}
	if strings.Contains(err.Error(), "requires root") {
		t.Fatalf("expected unsupported OS before root message, got: %v", err)
	}
}

func TestServeInstallUsesProvidedListenInUnit(t *testing.T) {
	tmpDir := t.TempDir()
	unitPath := filepath.Join(tmpDir, "cleanroom.service")

	prevEUID := serveInstallEUID
	prevGOOS := serveInstallGOOS
	prevSystemdPath := serveInstallSystemdUnitPath
	prevExecutable := serveInstallExecutablePath
	prevRunCommand := serveInstallRunCommand
	serveInstallEUID = func() int { return 0 }
	serveInstallGOOS = "linux"
	serveInstallSystemdUnitPath = unitPath
	serveInstallExecutablePath = func() (string, error) { return "/usr/local/bin/cleanroom", nil }
	serveInstallRunCommand = func(name string, args ...string) error { return nil }
	t.Cleanup(func() {
		serveInstallEUID = prevEUID
		serveInstallGOOS = prevGOOS
		serveInstallSystemdUnitPath = prevSystemdPath
		serveInstallExecutablePath = prevExecutable
		serveInstallRunCommand = prevRunCommand
	})

	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "install", Listen: "unix:///tmp/custom-cleanroom.sock"}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout}); err != nil {
		t.Fatalf("ServeCommand.Run returned error: %v", err)
	}

	raw, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read generated unit: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "--listen unix:///tmp/custom-cleanroom.sock") {
		t.Fatalf("expected provided --listen in unit, got:\n%s", content)
	}
	if strings.Contains(content, "--listen unix:///var/run/cleanroom/cleanroom.sock") {
		t.Fatalf("did not expect default listen when custom listen is provided, got:\n%s", content)
	}
}

func TestServeInstallDarwinDefaultsToUserScopeForNonRoot(t *testing.T) {
	tmpDir := t.TempDir()
	plistPath := filepath.Join(tmpDir, "Library", "LaunchAgents", launchdServiceName+".plist")

	prevEUID := serveInstallEUID
	prevUID := serveInstallUID
	prevGOOS := serveInstallGOOS
	prevUserHomeDir := serveInstallUserHomeDir
	prevExecutable := serveInstallExecutablePath
	prevRunCommand := serveInstallRunCommand
	serveInstallEUID = func() int { return 501 }
	serveInstallUID = func() int { return 501 }
	serveInstallGOOS = "darwin"
	serveInstallUserHomeDir = func() (string, error) { return tmpDir, nil }
	serveInstallExecutablePath = func() (string, error) { return "/usr/local/bin/cleanroom", nil }
	var calls [][]string
	serveInstallRunCommand = func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}
	t.Cleanup(func() {
		serveInstallEUID = prevEUID
		serveInstallUID = prevUID
		serveInstallGOOS = prevGOOS
		serveInstallUserHomeDir = prevUserHomeDir
		serveInstallExecutablePath = prevExecutable
		serveInstallRunCommand = prevRunCommand
	})

	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "install"}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout}); err != nil {
		t.Fatalf("ServeCommand.Run returned error: %v", err)
	}

	wantDomain := "gui/501"
	if !containsServeInstallCall(calls, []string{"launchctl", "bootstrap", wantDomain}) {
		t.Fatalf("expected launchctl bootstrap for user domain %q, calls=%v", wantDomain, calls)
	}
	if containsServeInstallCall(calls, []string{"launchctl", "bootstrap", "system"}) {
		t.Fatalf("did not expect launchctl bootstrap for system domain, calls=%v", calls)
	}
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("expected user launchd plist to be written at %s: %v", plistPath, err)
	}
	raw, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read user launchd plist: %v", err)
	}
	if strings.Contains(string(raw), "--listen unix:///var/run/cleanroom/cleanroom.sock") {
		t.Fatalf("did not expect user launchd plist to use system socket:\n%s", raw)
	}
}

func TestServeInstallDarwinSystemScopeUnsupported(t *testing.T) {
	prevEUID := serveInstallEUID
	prevGOOS := serveInstallGOOS
	serveInstallEUID = func() int { return 501 }
	serveInstallGOOS = "darwin"
	t.Cleanup(func() {
		serveInstallEUID = prevEUID
		serveInstallGOOS = prevGOOS
	})

	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "install", System: true}
	err := cmd.Run(&runtimeContext{CWD: t.TempDir(), Stdout: stdout})
	if err == nil {
		t.Fatal("expected unsupported --system error on darwin")
	}
	if !strings.Contains(err.Error(), "--system is unsupported on darwin") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestServeInstallDarwinRejectsRootByDefault(t *testing.T) {
	prevEUID := serveInstallEUID
	prevGOOS := serveInstallGOOS
	serveInstallEUID = func() int { return 0 }
	serveInstallGOOS = "darwin"
	t.Cleanup(func() {
		serveInstallEUID = prevEUID
		serveInstallGOOS = prevGOOS
	})

	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "install"}
	err := cmd.Run(&runtimeContext{CWD: t.TempDir(), Stdout: stdout})
	if err == nil {
		t.Fatal("expected root usage error on darwin")
	}
	if !strings.Contains(err.Error(), "run 'cleanroom serve' without sudo") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestServeInstallLinuxUserScopeUnsupported(t *testing.T) {
	prevEUID := serveInstallEUID
	prevGOOS := serveInstallGOOS
	serveInstallEUID = func() int { return 1000 }
	serveInstallGOOS = "linux"
	t.Cleanup(func() {
		serveInstallEUID = prevEUID
		serveInstallGOOS = prevGOOS
	})

	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "install", User: true}
	err := cmd.Run(&runtimeContext{CWD: t.TempDir(), Stdout: stdout})
	if err == nil {
		t.Fatal("expected unsupported --user error on linux")
	}
	if !strings.Contains(err.Error(), "--user is unsupported on linux") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestServeInstallRejectsUserAndSystemTogether(t *testing.T) {
	prevEUID := serveInstallEUID
	prevGOOS := serveInstallGOOS
	serveInstallEUID = func() int { return 501 }
	serveInstallGOOS = "darwin"
	t.Cleanup(func() {
		serveInstallEUID = prevEUID
		serveInstallGOOS = prevGOOS
	})

	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "install", User: true, System: true}
	err := cmd.Run(&runtimeContext{CWD: t.TempDir(), Stdout: stdout})
	if err == nil {
		t.Fatal("expected conflicting scope flags error")
	}
	if !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestServeInstallCanonicalizesRelativeTLSPaths(t *testing.T) {
	tmpDir := t.TempDir()
	unitPath := filepath.Join(tmpDir, "cleanroom.service")

	prevEUID := serveInstallEUID
	prevGOOS := serveInstallGOOS
	prevSystemdPath := serveInstallSystemdUnitPath
	prevExecutable := serveInstallExecutablePath
	prevRunCommand := serveInstallRunCommand
	serveInstallEUID = func() int { return 0 }
	serveInstallGOOS = "linux"
	serveInstallSystemdUnitPath = unitPath
	serveInstallExecutablePath = func() (string, error) { return "/usr/local/bin/cleanroom", nil }
	serveInstallRunCommand = func(name string, args ...string) error { return nil }
	t.Cleanup(func() {
		serveInstallEUID = prevEUID
		serveInstallGOOS = prevGOOS
		serveInstallSystemdUnitPath = prevSystemdPath
		serveInstallExecutablePath = prevExecutable
		serveInstallRunCommand = prevRunCommand
	})

	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{
		Action:  "install",
		TLSCert: "certs/server.pem",
		TLSKey:  "certs/server.key",
	}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout}); err != nil {
		t.Fatalf("ServeCommand.Run returned error: %v", err)
	}

	raw, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read generated unit: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "--tls-cert "+filepath.Join(tmpDir, "certs/server.pem")) {
		t.Fatalf("expected absolute --tls-cert path in unit, got:\n%s", content)
	}
	if !strings.Contains(content, "--tls-key "+filepath.Join(tmpDir, "certs/server.key")) {
		t.Fatalf("expected absolute --tls-key path in unit, got:\n%s", content)
	}
}

func TestServeInstallReturnsCommandErrors(t *testing.T) {
	tmpDir := t.TempDir()
	unitPath := filepath.Join(tmpDir, "cleanroom.service")

	prevEUID := serveInstallEUID
	prevGOOS := serveInstallGOOS
	prevSystemdPath := serveInstallSystemdUnitPath
	prevExecutable := serveInstallExecutablePath
	prevRunCommand := serveInstallRunCommand
	serveInstallEUID = func() int { return 0 }
	serveInstallGOOS = "linux"
	serveInstallSystemdUnitPath = unitPath
	serveInstallExecutablePath = func() (string, error) { return "/usr/local/bin/cleanroom", nil }
	serveInstallRunCommand = func(name string, args ...string) error {
		return errors.New("command failed")
	}
	t.Cleanup(func() {
		serveInstallEUID = prevEUID
		serveInstallGOOS = prevGOOS
		serveInstallSystemdUnitPath = prevSystemdPath
		serveInstallExecutablePath = prevExecutable
		serveInstallRunCommand = prevRunCommand
	})

	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "install"}
	err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout})
	if err == nil {
		t.Fatal("expected install command failure")
	}
	if !strings.Contains(err.Error(), "reload systemd") {
		t.Fatalf("expected daemon-reload context, got: %v", err)
	}
}

func TestServeUninstallRequiresRoot(t *testing.T) {
	prevEUID := serveInstallEUID
	prevGOOS := serveInstallGOOS
	serveInstallEUID = func() int { return 1000 }
	serveInstallGOOS = "linux"
	t.Cleanup(func() {
		serveInstallEUID = prevEUID
		serveInstallGOOS = prevGOOS
	})

	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "uninstall"}
	err := cmd.Run(&runtimeContext{CWD: t.TempDir(), Stdout: stdout})
	if err == nil {
		t.Fatal("expected root requirement error")
	}
	if !strings.Contains(err.Error(), "requires root") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "sudo cleanroom serve uninstall") {
		t.Fatalf("expected sudo guidance, got: %v", err)
	}
}

func TestServeUninstallRemovesServiceAndStops(t *testing.T) {
	tmpDir := t.TempDir()
	unitPath := filepath.Join(tmpDir, "cleanroom.service")
	if err := os.WriteFile(unitPath, []byte("existing-unit"), 0o644); err != nil {
		t.Fatalf("write existing unit: %v", err)
	}

	prevEUID := serveInstallEUID
	prevGOOS := serveInstallGOOS
	prevSystemdPath := serveInstallSystemdUnitPath
	prevRunCommand := serveInstallRunCommand
	prevRemoveFile := serveInstallRemoveFile
	serveInstallEUID = func() int { return 0 }
	serveInstallGOOS = "linux"
	serveInstallSystemdUnitPath = unitPath
	serveInstallRemoveFile = os.Remove
	var calls [][]string
	serveInstallRunCommand = func(name string, args ...string) error {
		call := append([]string{name}, args...)
		calls = append(calls, call)
		return nil
	}
	t.Cleanup(func() {
		serveInstallEUID = prevEUID
		serveInstallGOOS = prevGOOS
		serveInstallSystemdUnitPath = prevSystemdPath
		serveInstallRunCommand = prevRunCommand
		serveInstallRemoveFile = prevRemoveFile
	})

	stdout, readStdout := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "uninstall"}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout}); err != nil {
		t.Fatalf("ServeCommand.Run returned error: %v", err)
	}

	if _, err := os.Stat(unitPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected service file to be removed")
	}

	wantCalls := [][]string{
		{"systemctl", "stop", "cleanroom.service"},
		{"systemctl", "disable", "cleanroom.service"},
		{"systemctl", "daemon-reload"},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("unexpected systemctl commands: got %v want %v", calls, wantCalls)
	}

	out := readStdout()
	if !strings.Contains(out, "daemon uninstalled") {
		t.Fatalf("expected uninstalled message, got: %s", out)
	}
	if !strings.Contains(out, "manager=systemd") {
		t.Fatalf("expected manager=systemd, got: %s", out)
	}
}

func TestServeUninstallFailsWhenNotInstalled(t *testing.T) {
	tmpDir := t.TempDir()
	unitPath := filepath.Join(tmpDir, "cleanroom.service")

	prevEUID := serveInstallEUID
	prevGOOS := serveInstallGOOS
	prevSystemdPath := serveInstallSystemdUnitPath
	serveInstallEUID = func() int { return 0 }
	serveInstallGOOS = "linux"
	serveInstallSystemdUnitPath = unitPath
	t.Cleanup(func() {
		serveInstallEUID = prevEUID
		serveInstallGOOS = prevGOOS
		serveInstallSystemdUnitPath = prevSystemdPath
	})

	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "uninstall"}
	err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout})
	if err == nil {
		t.Fatal("expected error when service file doesn't exist")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestServeUninstallUnsupportedOS(t *testing.T) {
	prevGOOS := serveInstallGOOS
	serveInstallGOOS = "windows"
	t.Cleanup(func() {
		serveInstallGOOS = prevGOOS
	})

	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "uninstall"}
	err := cmd.Run(&runtimeContext{CWD: t.TempDir(), Stdout: stdout})
	if err == nil {
		t.Fatal("expected unsupported OS error")
	}
	if !strings.Contains(err.Error(), "unsupported on windows") {
		t.Fatalf("expected unsupported OS message, got: %v", err)
	}
}

func TestServeStatusSystemdInstalled(t *testing.T) {
	tmpDir := t.TempDir()
	unitPath := filepath.Join(tmpDir, "cleanroom.service")
	if err := os.WriteFile(unitPath, []byte("unit"), 0o644); err != nil {
		t.Fatalf("write unit: %v", err)
	}

	prevGOOS := serveInstallGOOS
	prevSystemdPath := serveInstallSystemdUnitPath
	prevRunCommand := serveInstallRunCommand
	serveInstallGOOS = "linux"
	serveInstallSystemdUnitPath = unitPath
	serveInstallRunCommand = func(name string, args ...string) error {
		return nil // all commands succeed (active + enabled)
	}
	t.Cleanup(func() {
		serveInstallGOOS = prevGOOS
		serveInstallSystemdUnitPath = prevSystemdPath
		serveInstallRunCommand = prevRunCommand
	})

	stdout, readStdout := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "status", System: true}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout}); err != nil {
		t.Fatalf("ServeCommand.Run returned error: %v", err)
	}

	out := readStdout()
	if !strings.Contains(out, "installed=true") {
		t.Fatalf("expected installed=true, got: %s", out)
	}
	if !strings.Contains(out, "active=active") {
		t.Fatalf("expected active=active, got: %s", out)
	}
	if !strings.Contains(out, "enabled=true") {
		t.Fatalf("expected enabled=true, got: %s", out)
	}
}

func TestServeStatusSystemdNotInstalled(t *testing.T) {
	tmpDir := t.TempDir()
	unitPath := filepath.Join(tmpDir, "cleanroom.service")

	prevGOOS := serveInstallGOOS
	prevSystemdPath := serveInstallSystemdUnitPath
	prevRunCommand := serveInstallRunCommand
	serveInstallGOOS = "linux"
	serveInstallSystemdUnitPath = unitPath
	serveInstallRunCommand = func(name string, args ...string) error {
		return &exec.ExitError{ProcessState: &os.ProcessState{}}
	}
	t.Cleanup(func() {
		serveInstallGOOS = prevGOOS
		serveInstallSystemdUnitPath = prevSystemdPath
		serveInstallRunCommand = prevRunCommand
	})

	stdout, readStdout := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "status", System: true}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout}); err != nil {
		t.Fatalf("ServeCommand.Run returned error: %v", err)
	}

	out := readStdout()
	if !strings.Contains(out, "installed=false") {
		t.Fatalf("expected installed=false, got: %s", out)
	}
	if !strings.Contains(out, "active=inactive") {
		t.Fatalf("expected active=inactive, got: %s", out)
	}
	if !strings.Contains(out, "enabled=false") {
		t.Fatalf("expected enabled=false, got: %s", out)
	}
}

func TestServeStatusUnsupportedOS(t *testing.T) {
	prevGOOS := serveInstallGOOS
	serveInstallGOOS = "windows"
	t.Cleanup(func() {
		serveInstallGOOS = prevGOOS
	})

	stdout, _ := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "status", System: true}
	err := cmd.Run(&runtimeContext{CWD: t.TempDir(), Stdout: stdout})
	if err == nil {
		t.Fatal("expected unsupported OS error")
	}
	if !strings.Contains(err.Error(), "unsupported on windows") {
		t.Fatalf("expected unsupported OS message, got: %v", err)
	}
}

func TestServeStatusLaunchdIncludesListenEndpoint(t *testing.T) {
	tmpDir := t.TempDir()
	plistPath := filepath.Join(tmpDir, "Library", "LaunchAgents", launchdServiceName+".plist")
	content := renderLaunchdService("/usr/local/bin/cleanroom", []string{"serve", "--listen", "unix:///tmp/custom-cleanroom.sock"})
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatalf("mkdir launch agents dir: %v", err)
	}
	if err := os.WriteFile(plistPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write launchd plist: %v", err)
	}

	prevGOOS := serveInstallGOOS
	prevEUID := serveInstallEUID
	prevUID := serveInstallUID
	prevUserHomeDir := serveInstallUserHomeDir
	prevRunCommand := serveInstallRunCommand
	prevRunCommandOutput := serveInstallRunCommandOutput
	serveInstallGOOS = "darwin"
	serveInstallEUID = func() int { return 501 }
	serveInstallUID = func() int { return 501 }
	serveInstallUserHomeDir = func() (string, error) { return tmpDir, nil }
	serveInstallRunCommand = func(name string, args ...string) error { return nil }
	serveInstallRunCommandOutput = func(name string, args ...string) (string, error) {
		return "state = running\n", nil
	}
	t.Cleanup(func() {
		serveInstallGOOS = prevGOOS
		serveInstallEUID = prevEUID
		serveInstallUID = prevUID
		serveInstallUserHomeDir = prevUserHomeDir
		serveInstallRunCommand = prevRunCommand
		serveInstallRunCommandOutput = prevRunCommandOutput
	})

	stdout, readStdout := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "status"}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout}); err != nil {
		t.Fatalf("ServeCommand.Run returned error: %v", err)
	}

	out := readStdout()
	if !strings.Contains(out, "listen=unix:///tmp/custom-cleanroom.sock") {
		t.Fatalf("expected launchd status to include configured listen endpoint, got: %s", out)
	}
}

func TestServeStatusLaunchdReportsInactiveWhenNotRunning(t *testing.T) {
	tmpDir := t.TempDir()
	plistPath := filepath.Join(tmpDir, "Library", "LaunchAgents", launchdServiceName+".plist")
	content := renderLaunchdService("/usr/local/bin/cleanroom", []string{"serve", "--listen", "unix:///tmp/custom-cleanroom.sock"})
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatalf("mkdir launch agents dir: %v", err)
	}
	if err := os.WriteFile(plistPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write launchd plist: %v", err)
	}

	prevGOOS := serveInstallGOOS
	prevEUID := serveInstallEUID
	prevUID := serveInstallUID
	prevUserHomeDir := serveInstallUserHomeDir
	prevRunCommandOutput := serveInstallRunCommandOutput
	serveInstallGOOS = "darwin"
	serveInstallEUID = func() int { return 501 }
	serveInstallUID = func() int { return 501 }
	serveInstallUserHomeDir = func() (string, error) { return tmpDir, nil }
	serveInstallRunCommandOutput = func(name string, args ...string) (string, error) {
		return "state = spawn scheduled\n", nil
	}
	t.Cleanup(func() {
		serveInstallGOOS = prevGOOS
		serveInstallEUID = prevEUID
		serveInstallUID = prevUID
		serveInstallUserHomeDir = prevUserHomeDir
		serveInstallRunCommandOutput = prevRunCommandOutput
	})

	stdout, readStdout := makeStdoutCapture(t)
	cmd := &ServeCommand{Action: "status"}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout}); err != nil {
		t.Fatalf("ServeCommand.Run returned error: %v", err)
	}

	out := readStdout()
	if !strings.Contains(out, "active=inactive") {
		t.Fatalf("expected launchd status to report inactive for non-running state, got: %s", out)
	}
	if !strings.Contains(out, "state=spawn scheduled") {
		t.Fatalf("expected launchd status to include raw launchd state, got: %s", out)
	}
}

func TestJoinSystemdExecArgsQuotesSingleQuoteArgs(t *testing.T) {
	joined := joinSystemdExecArgs([]string{
		"/usr/local/bin/cleanroom",
		"serve",
		"--tls-key",
		"/etc/cleanroom/bob's-key.pem",
	})

	if !strings.Contains(joined, "\"/etc/cleanroom/bob's-key.pem\"") {
		t.Fatalf("expected single-quote arg to be quoted, got: %q", joined)
	}
}

func TestRenderLaunchdServiceOmitsEnvironmentVariables(t *testing.T) {
	plist := renderLaunchdService("/usr/local/bin/cleanroom", []string{"serve"})
	if strings.Contains(plist, "EnvironmentVariables") {
		t.Fatalf("expected launchd plist to omit EnvironmentVariables, got:\n%s", plist)
	}
}

func TestLaunchdProgramArgumentsRejectsNonArrayValue(t *testing.T) {
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.buildkite.cleanroom</string>
	<key>ProgramArguments</key>
	<dict>
		<key>unexpected</key>
		<string>value</string>
	</dict>
	<key>EnvironmentVariables</key>
	<array>
		<string>SHOULD_NOT_BE_PARSED</string>
	</array>
</dict>
</plist>`

	_, err := launchdProgramArguments([]byte(plist))
	if err == nil {
		t.Fatal("expected ProgramArguments parsing error when value is not an array")
	}
	if !strings.Contains(err.Error(), "must be an array") {
		t.Fatalf("expected non-array ProgramArguments error, got: %v", err)
	}
}

func containsServeInstallCall(calls [][]string, wantPrefix []string) bool {
	for _, call := range calls {
		if len(call) < len(wantPrefix) {
			continue
		}
		match := true
		for i := range wantPrefix {
			if call[i] != wantPrefix[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
