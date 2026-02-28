package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"gopkg.in/yaml.v3"
)

func TestConfigInitWritesRuntimeConfig(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	stdout, _ := makeStdoutCapture(t)
	cmd := &ConfigInitCommand{}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout}); err != nil {
		t.Fatalf("ConfigInitCommand.Run returned error: %v", err)
	}

	configPath := filepath.Join(tmpDir, "cleanroom", "config.yaml")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}

	var cfg runtimeconfig.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse generated yaml: %v", err)
	}
	if !strings.Contains(string(raw), "darwin-vz:") {
		t.Fatalf("expected generated config to use backends.darwin-vz key, got:\n%s", raw)
	}
	if got := strings.TrimSpace(cfg.DefaultBackend); got == "" {
		t.Fatal("expected default_backend to be populated")
	}
	if got := strings.TrimSpace(cfg.Backends.Firecracker.BinaryPath); got == "" {
		t.Fatal("expected backends.firecracker.binary_path to be populated")
	}
	if got := strings.TrimSpace(cfg.Backends.Firecracker.KernelImage); got != "" {
		t.Fatalf("expected backends.firecracker.kernel_image to default empty, got %q", got)
	}
	if got := strings.TrimSpace(cfg.Backends.DarwinVZ.KernelImage); got != "" {
		t.Fatalf("expected backends.darwin-vz.kernel_image to default empty, got %q", got)
	}
	if got, want := cfg.Backends.Firecracker.Services.Docker.StartupTimeoutSeconds, int64(20); got != want {
		t.Fatalf("expected backends.firecracker.services.docker.startup_timeout_seconds=%d, got %d", want, got)
	}
	if got, want := cfg.Backends.Firecracker.Services.Docker.StorageDriver, "vfs"; got != want {
		t.Fatalf("expected backends.firecracker.services.docker.storage_driver=%q, got %q", want, got)
	}
	if cfg.Backends.Firecracker.Services.Docker.IPTables {
		t.Fatal("expected backends.firecracker.services.docker.iptables to default false")
	}
}

func TestConfigInitRefusesOverwriteWithoutForce(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	original := "existing: true\n"
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatalf("write existing config: %v", err)
	}

	stdout, _ := makeStdoutCapture(t)
	cmd := &ConfigInitCommand{Path: configPath}
	err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout})
	if err == nil {
		t.Fatal("expected overwrite error")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("unexpected error: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read existing config: %v", err)
	}
	if got, want := string(raw), original; got != want {
		t.Fatalf("config changed unexpectedly: got %q want %q", got, want)
	}
}

func TestConfigInitForceOverwritesExistingFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "runtime", "cleanroom.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("existing: true\n"), 0o644); err != nil {
		t.Fatalf("write existing config: %v", err)
	}

	stdout, _ := makeStdoutCapture(t)
	cmd := &ConfigInitCommand{
		Path:  filepath.Join("runtime", "cleanroom.yaml"),
		Force: true,
	}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout}); err != nil {
		t.Fatalf("ConfigInitCommand.Run returned error: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read overwritten config: %v", err)
	}
	if strings.Contains(string(raw), "existing: true") {
		t.Fatalf("expected config to be overwritten, got:\n%s", raw)
	}
}
