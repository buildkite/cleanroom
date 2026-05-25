//go:build unix

package scripts_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInstallScriptPromptDeclinesSilentlyWithoutTTY(t *testing.T) {
	content, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}

	script := shellFunction(t, string(content), "prompt_install_homebrew_package") + "\nprompt_install_homebrew_package e2fsprogs\n"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("prompt_install_homebrew_package hung without a tty; output: %q", string(out))
	}
	if err == nil {
		t.Fatal("expected prompt_install_homebrew_package to decline without a tty")
	}
	if got := string(out); strings.TrimSpace(got) != "" {
		t.Fatalf("expected missing tty prompt to be silent, got %q", got)
	}
}

func TestInstallScriptInstallsMacOSUserDaemonWithoutSudo(t *testing.T) {
	content, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}

	tmpDir := t.TempDir()
	cleanroomPath := filepath.Join(tmpDir, "cleanroom")
	callsPath := filepath.Join(tmpDir, "cleanroom-calls")
	writeInstallTestExecutable(t, cleanroomPath, `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$CLEANROOM_CALLS"
`)

	script := strings.Join([]string{
		shellFunction(t, string(content), "log"),
		shellFunction(t, string(content), "warn"),
		shellFunction(t, string(content), "die"),
		shellFunction(t, string(content), "install_cleanroom_daemon"),
		"id() { printf '501\n'; }",
		`HOST_OS=Darwin`,
		`INSTALL_DAEMON=1`,
		`INSTALL_DIR="$TEST_INSTALL_DIR"`,
		`install_cleanroom_daemon`,
	}, "\n")

	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "TEST_INSTALL_DIR="+tmpDir, "CLEANROOM_CALLS="+callsPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install_cleanroom_daemon failed: %v\n%s", err, out)
	}

	got := strings.TrimSpace(readFile(t, callsPath))
	if got != "daemon install --init-config --restart" {
		t.Fatalf("unexpected cleanroom daemon command: got %q", got)
	}
}

func TestInstallScriptInstallsLinuxDaemonWithSudoWhenNonRoot(t *testing.T) {
	content, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}

	tmpDir := t.TempDir()
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("create fake bin dir: %v", err)
	}
	cleanroomPath := filepath.Join(tmpDir, "cleanroom")
	sudoCallsPath := filepath.Join(tmpDir, "sudo-calls")
	writeInstallTestExecutable(t, cleanroomPath, "#!/usr/bin/env bash\nexit 0\n")
	writeInstallTestExecutable(t, filepath.Join(binDir, "sudo"), `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$SUDO_CALLS"
`)

	script := strings.Join([]string{
		shellFunction(t, string(content), "log"),
		shellFunction(t, string(content), "warn"),
		shellFunction(t, string(content), "die"),
		shellFunction(t, string(content), "install_cleanroom_daemon"),
		"id() { printf '1000\n'; }",
		`HOST_OS=Linux`,
		`INSTALL_DAEMON=1`,
		`INSTALL_DIR="$TEST_INSTALL_DIR"`,
		`install_cleanroom_daemon`,
	}, "\n")

	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(
		os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TEST_INSTALL_DIR="+tmpDir,
		"SUDO_CALLS="+sudoCallsPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install_cleanroom_daemon failed: %v\n%s", err, out)
	}

	got := strings.TrimSpace(readFile(t, sudoCallsPath))
	want := cleanroomPath + " daemon install --init-config --restart"
	if got != want {
		t.Fatalf("unexpected sudo daemon command: got %q want %q", got, want)
	}
}

func TestInstallScriptRemovesDarwinGuestAgentArchAlias(t *testing.T) {
	content, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}

	tmpDir := t.TempDir()
	aliasPath := filepath.Join(tmpDir, "cleanroom-guest-agent-linux-arm64")
	if err := os.WriteFile(aliasPath, []byte("stale guest-agent"), 0o755); err != nil {
		t.Fatalf("write guest agent alias: %v", err)
	}

	script := strings.Join([]string{
		"declare -a SUDO_CMD=()",
		shellFunction(t, string(content), "run_with_optional_sudo"),
		shellFunction(t, string(content), "remove_legacy_darwin_guest_agent_arch_alias"),
		`HOST_OS=Darwin`,
		`HOST_GOARCH=arm64`,
		`INSTALL_DIR="$TEST_INSTALL_DIR"`,
		`remove_legacy_darwin_guest_agent_arch_alias`,
	}, "\n")

	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "TEST_INSTALL_DIR="+tmpDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("remove_legacy_darwin_guest_agent_arch_alias failed: %v\n%s", err, out)
	}

	if _, err := os.Stat(aliasPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected guest agent alias to be removed, got err %v", err)
	}
}

func shellFunction(t *testing.T, content, name string) string {
	t.Helper()

	start := strings.Index(content, name+"() {")
	if start < 0 {
		t.Fatalf("function %s not found", name)
	}
	rest := content[start:]
	end := strings.Index(rest, "\n}\n\n")
	if end < 0 {
		t.Fatalf("function %s terminator not found", name)
	}
	return rest[:end+3]
}

func writeInstallTestExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
