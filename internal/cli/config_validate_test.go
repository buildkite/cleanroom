package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	backendfirecracker "github.com/buildkite/cleanroom/internal/backend/firecracker"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

func TestConfigValidateReportsResolvedPathAndBackend(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("default_backend: firecracker\nbackends:\n  firecracker:\n    binary_path: firecracker\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	stdout, readStdout := makeStdoutCapture(t)
	cmd := &ConfigValidateCommand{Path: configPath}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout}); err != nil {
		t.Fatalf("ConfigValidateCommand.Run returned error: %v", err)
	}

	out := readStdout()
	if !strings.Contains(out, "runtime config valid") {
		t.Fatalf("expected validation status, got %q", out)
	}
	if !strings.Contains(out, configPath) {
		t.Fatalf("expected resolved config path in output, got %q", out)
	}
	if !strings.Contains(out, "default backend") || !strings.Contains(out, "firecracker") {
		t.Fatalf("expected default backend details in output, got %q", out)
	}
}

func TestConfigValidateJSONIncludesConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("default_backend: darwin-vz\nbackends:\n  darwin-vz:\n    network:\n      mode: filehandle\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	stdout, readStdout := makeStdoutCapture(t)
	cmd := &ConfigValidateCommand{Path: configPath, JSON: true}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout}); err != nil {
		t.Fatalf("ConfigValidateCommand.Run returned error: %v", err)
	}

	var payload struct {
		Path           string `json:"path"`
		DefaultBackend string `json:"default_backend"`
	}
	if err := json.Unmarshal([]byte(readStdout()), &payload); err != nil {
		t.Fatalf("parse config validate json: %v", err)
	}
	if got, want := payload.Path, configPath; got != want {
		t.Fatalf("unexpected path: got %q want %q", got, want)
	}
	if got, want := payload.DefaultBackend, "darwin-vz"; got != want {
		t.Fatalf("unexpected default backend: got %q want %q", got, want)
	}
}

func TestConfigValidateRejectsMissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	cmd := &ConfigValidateCommand{Path: filepath.Join(tmpDir, "missing.yaml")}
	stdout, _ := makeStdoutCapture(t)
	err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout})
	if err == nil {
		t.Fatal("expected missing config error")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConfigValidateRejectsInvalidSandboxLifecycleConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	content := `default_backend: firecracker
sandbox_lifecycle:
  idle_suspend_after_seconds: -1
backends:
  firecracker:
    binary_path: firecracker
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	stdout, _ := makeStdoutCapture(t)
	cmd := &ConfigValidateCommand{Path: configPath}
	err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout})
	if err == nil {
		t.Fatal("expected invalid lifecycle config error")
	}
	if !strings.Contains(err.Error(), "sandbox_lifecycle.idle_suspend_after_seconds") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunConfigValidateBypassesBrokenDefaultRuntimeConfig(t *testing.T) {
	tmpDir := t.TempDir()
	xdgConfigHome := filepath.Join(tmpDir, "xdg")
	defaultConfigPath := filepath.Join(xdgConfigHome, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(defaultConfigPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(defaultConfigPath, []byte("default_backend: broken\n"), 0o644); err != nil {
		t.Fatalf("write broken default config: %v", err)
	}

	explicitConfigPath := filepath.Join(tmpDir, "explicit.yaml")
	if err := os.WriteFile(explicitConfigPath, []byte("default_backend: firecracker\nbackends:\n  firecracker:\n    binary_path: firecracker\n"), 0o644); err != nil {
		t.Fatalf("write explicit config: %v", err)
	}

	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)
	if err := Run([]string{"config", "validate", "--path", explicitConfigPath}, "dev"); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestRunConfigInitBypassesBrokenDefaultRuntimeConfig(t *testing.T) {
	stubFirecrackerHostSupport(t, func(context.Context, backend.FirecrackerConfig) backendfirecracker.HostSupport {
		return backendfirecracker.HostSupport{}
	})

	tmpDir := t.TempDir()
	xdgConfigHome := filepath.Join(tmpDir, "xdg")
	defaultConfigPath := filepath.Join(xdgConfigHome, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(defaultConfigPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(defaultConfigPath, []byte("default_backend: broken\n"), 0o644); err != nil {
		t.Fatalf("write broken default config: %v", err)
	}

	outputPath := filepath.Join(tmpDir, "fresh.yaml")
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)
	if err := Run([]string{"config", "init", "--path", outputPath, "--force"}, "dev"); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if _, err := os.Stat(outputPath); err != nil {
		t.Fatalf("expected config init output at %s: %v", outputPath, err)
	}
}

func TestRunConfigInitForceOverwritesMalformedDefaultRuntimeConfig(t *testing.T) {
	stubFirecrackerHostSupport(t, func(context.Context, backend.FirecrackerConfig) backendfirecracker.HostSupport {
		return backendfirecracker.HostSupport{}
	})

	tmpDir := t.TempDir()
	xdgConfigHome := filepath.Join(tmpDir, "xdg")
	defaultConfigPath := filepath.Join(xdgConfigHome, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(defaultConfigPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(defaultConfigPath, []byte("default_backend: [\n"), 0o644); err != nil {
		t.Fatalf("write malformed default config: %v", err)
	}

	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)
	if err := Run([]string{"config", "init", "--force"}, "dev"); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	cfg, resolvedPath, err := runtimeconfig.LoadPath(defaultConfigPath)
	if err != nil {
		t.Fatalf("expected overwritten config at %s to be valid: %v", defaultConfigPath, err)
	}
	if got, want := resolvedPath, defaultConfigPath; got != want {
		t.Fatalf("unexpected resolved path: got %q want %q", got, want)
	}
	if strings.TrimSpace(cfg.DefaultBackend) == "" {
		t.Fatal("expected overwritten config to set default backend")
	}
}
