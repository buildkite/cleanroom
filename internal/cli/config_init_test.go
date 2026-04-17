package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	backendfirecracker "github.com/buildkite/cleanroom/internal/backend/firecracker"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"gopkg.in/yaml.v3"
)

func TestConfigInitWritesRuntimeConfig(t *testing.T) {
	stubFirecrackerHostSupport(t, func(context.Context, backend.FirecrackerConfig) backendfirecracker.HostSupport {
		return backendfirecracker.HostSupport{
			RuntimeUsable:   false,
			SnapshotsUsable: false,
			SnapshotMessage: "machine bootstrap incomplete",
		}
	})

	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := &ConfigInitCommand{}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout, Stderr: stderr}); err != nil {
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
	if got, want := cfg.Backends.Firecracker.Snapshots.Driver, "file"; got != want {
		t.Fatalf("expected backends.firecracker.snapshots.driver=%q, got %q", want, got)
	}
	if cfg.Backends.Firecracker.Snapshots.Enabled {
		t.Fatal("expected backends.firecracker.snapshots.enabled to default false")
	}
	if got, want := cfg.Backends.DarwinVZ.Snapshots.Driver, "apfs"; got != want {
		t.Fatalf("expected backends.darwin-vz.snapshots.driver=%q, got %q", want, got)
	}
	if cfg.Backends.DarwinVZ.Snapshots.Enabled {
		t.Fatal("expected backends.darwin-vz.snapshots.enabled to default false")
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
	if !strings.Contains(string(raw), "snapshots:") {
		t.Fatalf("expected generated config to include snapshot defaults, got:\n%s", raw)
	}
	if !strings.Contains(string(raw), "enabled: false") {
		t.Fatalf("expected generated config to include disabled snapshot default, got:\n%s", raw)
	}
	if !strings.Contains(string(raw), "driver: file") {
		t.Fatalf("expected generated config to include firecracker snapshot driver default, got:\n%s", raw)
	}
	if !strings.Contains(string(raw), "driver: apfs") {
		t.Fatalf("expected generated config to include darwin-vz snapshot driver default, got:\n%s", raw)
	}
	if strings.Contains(string(raw), "base_dir:") {
		t.Fatalf("expected generated config to omit empty snapshot base_dir, got:\n%s", raw)
	}
	if strings.Contains(string(raw), "zfs_dataset:") {
		t.Fatalf("expected generated config to omit empty snapshot zfs_dataset, got:\n%s", raw)
	}
	if strings.Contains(string(raw), "quiesce_timeout_seconds:") {
		t.Fatalf("expected generated config to omit empty snapshot quiesce timeout, got:\n%s", raw)
	}
}

func TestConfigInitRefusesOverwriteWithoutForce(t *testing.T) {
	stubFirecrackerHostSupport(t, func(context.Context, backend.FirecrackerConfig) backendfirecracker.HostSupport {
		return backendfirecracker.HostSupport{}
	})

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
	stubFirecrackerHostSupport(t, func(context.Context, backend.FirecrackerConfig) backendfirecracker.HostSupport {
		return backendfirecracker.HostSupport{}
	})

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

func TestConfigInitUsesFileSnapshotsWhenZFSUnavailable(t *testing.T) {
	stubFirecrackerHostSupport(t, func(context.Context, backend.FirecrackerConfig) backendfirecracker.HostSupport {
		return backendfirecracker.HostSupport{
			RuntimeUsable:   true,
			SnapshotsUsable: true,
			ZFSUsable:       false,
			ZFSMessage:      "no existing Cleanroom ZFS dataset root matched cleanroom or */cleanroom",
		}
	})

	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	stdout, _ := makeStdoutCapture(t)
	stderr, readStderr := makeStdoutCapture(t)
	cmd := &ConfigInitCommand{DefaultBackend: "firecracker"}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout, Stderr: stderr}); err != nil {
		t.Fatalf("ConfigInitCommand.Run returned error: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(tmpDir, "cleanroom", "config.yaml"))
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}

	var cfg runtimeconfig.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse generated yaml: %v", err)
	}
	if !cfg.Backends.Firecracker.Snapshots.Enabled {
		t.Fatal("expected firecracker snapshots to default enabled when snapshot runtime is usable")
	}
	if got, want := cfg.Backends.Firecracker.Snapshots.Driver, "file"; got != want {
		t.Fatalf("unexpected firecracker snapshot driver: got %q want %q", got, want)
	}
	if cfg.Backends.Firecracker.Snapshots.ZFSDataset != "" {
		t.Fatalf("expected firecracker zfs dataset to be omitted, got %q", cfg.Backends.Firecracker.Snapshots.ZFSDataset)
	}
	if out := readStderr(); !strings.Contains(out, "driver=file") || !strings.Contains(out, "no existing Cleanroom ZFS dataset root matched cleanroom or */cleanroom") {
		t.Fatalf("expected file-driver warning, got %q", out)
	}
}

func TestConfigInitUsesZFSWhenCleanroomDatasetDetected(t *testing.T) {
	stubFirecrackerHostSupport(t, func(context.Context, backend.FirecrackerConfig) backendfirecracker.HostSupport {
		return backendfirecracker.HostSupport{
			RuntimeUsable:   true,
			SnapshotsUsable: true,
			ZFSUsable:       true,
			ZFSDatasetRoot:  "cleanroom",
			ZFSMessage:      "auto-detected Cleanroom ZFS dataset root \"cleanroom\"",
		}
	})

	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	stdout, _ := makeStdoutCapture(t)
	stderr, readStderr := makeStdoutCapture(t)
	cmd := &ConfigInitCommand{DefaultBackend: "firecracker"}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout, Stderr: stderr}); err != nil {
		t.Fatalf("ConfigInitCommand.Run returned error: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(tmpDir, "cleanroom", "config.yaml"))
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}

	var cfg runtimeconfig.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse generated yaml: %v", err)
	}
	if !cfg.Backends.Firecracker.Snapshots.Enabled {
		t.Fatal("expected firecracker snapshots enabled for zfs-capable host runtime")
	}
	if got, want := cfg.Backends.Firecracker.Snapshots.Driver, "zfs"; got != want {
		t.Fatalf("unexpected firecracker snapshot driver: got %q want %q", got, want)
	}
	if got, want := cfg.Backends.Firecracker.Snapshots.ZFSDataset, "cleanroom"; got != want {
		t.Fatalf("unexpected firecracker zfs dataset: got %q want %q", got, want)
	}
	if out := strings.TrimSpace(readStderr()); out != "" {
		t.Fatalf("expected no warning output, got %q", out)
	}
}

func TestConfigInitWarnsWhenZFSDatasetDetectionIsAmbiguous(t *testing.T) {
	stubFirecrackerHostSupport(t, func(context.Context, backend.FirecrackerConfig) backendfirecracker.HostSupport {
		return backendfirecracker.HostSupport{
			RuntimeUsable:   true,
			SnapshotsUsable: true,
			ZFSUsable:       false,
			ZFSMessage:      "multiple Cleanroom ZFS dataset roots detected: tank/ci/cleanroom, tank/dev/cleanroom",
		}
	})

	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	stdout, _ := makeStdoutCapture(t)
	stderr, readStderr := makeStdoutCapture(t)
	cmd := &ConfigInitCommand{DefaultBackend: "firecracker"}
	if err := cmd.Run(&runtimeContext{CWD: tmpDir, Stdout: stdout, Stderr: stderr}); err != nil {
		t.Fatalf("ConfigInitCommand.Run returned error: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(tmpDir, "cleanroom", "config.yaml"))
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}

	var cfg runtimeconfig.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse generated yaml: %v", err)
	}
	if got, want := cfg.Backends.Firecracker.Snapshots.Driver, "file"; got != want {
		t.Fatalf("unexpected firecracker snapshot driver: got %q want %q", got, want)
	}
	if !cfg.Backends.Firecracker.Snapshots.Enabled {
		t.Fatal("expected firecracker snapshots enabled with file fallback when zfs detection is ambiguous")
	}
	if out := readStderr(); !strings.Contains(out, "multiple Cleanroom ZFS dataset roots detected") || !strings.Contains(out, "tank/ci/cleanroom") {
		t.Fatalf("expected ambiguous zfs warning, got %q", out)
	}
}
