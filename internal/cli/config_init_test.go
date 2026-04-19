package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	backenddarwinvz "github.com/buildkite/cleanroom/internal/backend/darwinvz"
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
	stubDarwinVZSnapshotSupport(t, func() backenddarwinvz.SnapshotSupport {
		return backenddarwinvz.SnapshotSupport{Usable: true, Message: "darwin-vz snapshot runtime is usable"}
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
	if runtime.GOOS == "darwin" {
		if !strings.Contains(string(raw), "darwin-vz:") {
			t.Fatalf("expected generated config to use backends.darwin-vz key, got:\n%s", raw)
		}
		if !strings.Contains(string(raw), "backends:\n  darwin-vz:") {
			t.Fatalf("expected generated config to use 2-space indentation, got:\n%s", raw)
		}
	} else {
		if !strings.Contains(string(raw), "firecracker:") {
			t.Fatalf("expected generated config to use backends.firecracker key, got:\n%s", raw)
		}
		if !strings.Contains(string(raw), "backends:\n  firecracker:") {
			t.Fatalf("expected generated config to use 2-space indentation, got:\n%s", raw)
		}
	}
	if got := strings.TrimSpace(cfg.DefaultBackend); got == "" {
		t.Fatal("expected default_backend to be populated")
	}
	if runtime.GOOS == "darwin" {
		if strings.Contains(string(raw), "firecracker:") {
			t.Fatalf("expected generated config to omit firecracker backend on darwin hosts, got:\n%s", raw)
		}
		if got := strings.TrimSpace(cfg.Backends.DarwinVZ.KernelImage); got != "" {
			t.Fatalf("expected backends.darwin-vz.kernel_image to default empty, got %q", got)
		}
		if got, want := cfg.Backends.DarwinVZ.Snapshots.Driver, "apfs"; got != want {
			t.Fatalf("expected backends.darwin-vz.snapshots.driver=%q, got %q", want, got)
		}
		if !cfg.Backends.DarwinVZ.Snapshots.Enabled {
			t.Fatal("expected backends.darwin-vz.snapshots.enabled to default true on darwin")
		}
		if got, want := cfg.Backends.DarwinVZ.MemoryMiB, int64(4096); got != want {
			t.Fatalf("expected backends.darwin-vz.memory_mib=%d, got %d", want, got)
		}
		if got, want := int64(cfg.Backends.DarwinVZ.MinimumRootFSBytes), int64(4<<30); got != want {
			t.Fatalf("expected backends.darwin-vz.minimum_rootfs_bytes=%d, got %d", want, got)
		}
		if !strings.Contains(string(raw), "memory_mib: 4096") {
			t.Fatalf("expected generated config to include memory_mib: 4096, got:\n%s", raw)
		}
		if !strings.Contains(string(raw), "minimum_rootfs_bytes: 4GiB") {
			t.Fatalf("expected generated config to include minimum_rootfs_bytes: 4GiB, got:\n%s", raw)
		}
		for _, forbidden := range []string{"kernel_image:", "rootfs:", "iptables:"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("expected generated config to omit zero-value field %q, got:\n%s", forbidden, raw)
			}
		}
	} else {
		if strings.Contains(string(raw), "darwin-vz:") {
			t.Fatalf("expected generated config to omit darwin-vz backend on non-darwin hosts, got:\n%s", raw)
		}
		if got := strings.TrimSpace(cfg.Backends.Firecracker.BinaryPath); got == "" {
			t.Fatal("expected backends.firecracker.binary_path to be populated")
		}
		if got := strings.TrimSpace(cfg.Backends.Firecracker.KernelImage); got != "" {
			t.Fatalf("expected backends.firecracker.kernel_image to default empty, got %q", got)
		}
		if got, want := cfg.Backends.Firecracker.Snapshots.Driver, "file"; got != want {
			t.Fatalf("expected backends.firecracker.snapshots.driver=%q, got %q", want, got)
		}
		if cfg.Backends.Firecracker.Snapshots.Enabled {
			t.Fatal("expected backends.firecracker.snapshots.enabled to default false")
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
		for _, forbidden := range []string{"kernel_image:", "rootfs:", "iptables:"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("expected generated config to omit zero-value field %q, got:\n%s", forbidden, raw)
			}
		}
	}
	if !strings.Contains(string(raw), "snapshots:") {
		t.Fatalf("expected generated config to include snapshot defaults, got:\n%s", raw)
	}
	if runtime.GOOS != "darwin" && !strings.Contains(string(raw), "enabled: false") {
		t.Fatalf("expected generated config to include disabled snapshot default, got:\n%s", raw)
	}
	if runtime.GOOS != "darwin" && !strings.Contains(string(raw), "driver: file") {
		t.Fatalf("expected generated config to include firecracker snapshot driver default, got:\n%s", raw)
	}
	if runtime.GOOS == "darwin" && (!strings.Contains(string(raw), "enabled: true") || !strings.Contains(string(raw), "driver: apfs")) {
		t.Fatalf("expected generated config to enable darwin-vz snapshots on darwin hosts, got:\n%s", raw)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(string(raw), "driver: apfs") {
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

func TestConfigInitDisablesDarwinVZSnapshotsWhenSupportUnavailable(t *testing.T) {
	stubFirecrackerHostSupport(t, func(context.Context, backend.FirecrackerConfig) backendfirecracker.HostSupport {
		return backendfirecracker.HostSupport{}
	})
	stubDarwinVZSnapshotSupport(t, func() backenddarwinvz.SnapshotSupport {
		return backenddarwinvz.SnapshotSupport{Usable: false, Message: "darwin-vz snapshots remain disabled: helper unavailable"}
	})

	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	stdout, _ := makeStdoutCapture(t)
	stderr, readStderr := makeStdoutCapture(t)
	cmd := &ConfigInitCommand{DefaultBackend: "darwin-vz"}
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
	if !strings.Contains(string(raw), "darwin-vz:") {
		t.Fatalf("expected generated config to include darwin-vz backend, got:\n%s", raw)
	}
	if !strings.Contains(string(raw), "backends:\n  darwin-vz:") {
		t.Fatalf("expected generated config to use 2-space indentation, got:\n%s", raw)
	}
	if strings.Contains(string(raw), "firecracker:") {
		t.Fatalf("expected generated config to omit firecracker backend when default backend is darwin-vz, got:\n%s", raw)
	}
	if cfg.Backends.DarwinVZ.Snapshots.Enabled {
		t.Fatal("expected backends.darwin-vz.snapshots.enabled to default false when support is unavailable")
	}
	if got, want := cfg.Backends.DarwinVZ.Snapshots.Driver, "apfs"; got != want {
		t.Fatalf("unexpected darwin-vz snapshot driver: got %q want %q", got, want)
	}
	if got, want := cfg.Backends.DarwinVZ.MemoryMiB, int64(4096); got != want {
		t.Fatalf("expected backends.darwin-vz.memory_mib=%d, got %d", want, got)
	}
	if got, want := int64(cfg.Backends.DarwinVZ.MinimumRootFSBytes), int64(4<<30); got != want {
		t.Fatalf("expected backends.darwin-vz.minimum_rootfs_bytes=%d, got %d", want, got)
	}
	if out := readStderr(); !strings.Contains(out, "darwin-vz snapshots remain disabled: helper unavailable") {
		t.Fatalf("expected darwin-vz snapshot warning, got %q", out)
	}
	if !strings.Contains(string(raw), "memory_mib: 4096") {
		t.Fatalf("expected generated config to include memory_mib: 4096, got:\n%s", raw)
	}
	if !strings.Contains(string(raw), "minimum_rootfs_bytes: 4GiB") {
		t.Fatalf("expected generated config to include minimum_rootfs_bytes: 4GiB, got:\n%s", raw)
	}
	for _, forbidden := range []string{"enabled: false", "kernel_image:", "rootfs:", "iptables:"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("expected generated config to omit zero-value field %q, got:\n%s", forbidden, raw)
		}
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
