package cli

import (
	"errors"
	"os"
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
	serveInstallEUID = func() int { return 1000 }
	t.Cleanup(func() {
		serveInstallEUID = prevEUID
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
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("unexpected systemctl commands: got %v want %v", calls, wantCalls)
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

func TestServeInstallUsesTLSCADashedFlagInUnit(t *testing.T) {
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
	cmd := &ServeCommand{Action: "install", TLSCA: "/etc/cleanroom/ca.pem"}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout}); err != nil {
		t.Fatalf("ServeCommand.Run returned error: %v", err)
	}

	raw, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read generated unit: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "--tls-ca /etc/cleanroom/ca.pem") {
		t.Fatalf("expected --tls-ca flag in unit, got:\n%s", content)
	}
	if strings.Contains(content, "--tlsca") {
		t.Fatalf("did not expect legacy --tlsca flag in unit, got:\n%s", content)
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
