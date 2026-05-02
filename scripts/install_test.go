//go:build unix

package scripts_test

import (
	"context"
	"os"
	"os/exec"
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
