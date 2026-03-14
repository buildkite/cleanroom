package runtimeconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSupportsDarwinVZHyphenKey(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: darwin-vz
backends:
  darwin-vz:
    kernel_image: /tmp/kernel
    rootfs: /tmp/rootfs
    services:
      docker:
        startup_timeout_seconds: 25
        storage_driver: overlay2
        iptables: true
    vcpus: 2
    memory_mib: 1024
    guest_port: 10700
    launch_seconds: 30
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := cfg.Backends.DarwinVZ.KernelImage, "/tmp/kernel"; got != want {
		t.Fatalf("unexpected darwin-vz kernel: got %q want %q", got, want)
	}
	if got, want := cfg.Backends.DarwinVZ.Services.Docker.StartupTimeoutSeconds, int64(25); got != want {
		t.Fatalf("unexpected docker startup timeout: got %d want %d", got, want)
	}
	if got, want := cfg.Backends.DarwinVZ.Services.Docker.StorageDriver, "overlay2"; got != want {
		t.Fatalf("unexpected docker storage driver: got %q want %q", got, want)
	}
	if !cfg.Backends.DarwinVZ.Services.Docker.IPTables {
		t.Fatal("expected docker iptables to be enabled")
	}
}

func TestLoadSupportsLegacyDarwinVZUnderscoreKey(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: darwin-vz
backends:
  darwin_vz:
    kernel_image: /tmp/legacy-kernel
    rootfs: /tmp/legacy-rootfs
    vcpus: 4
    memory_mib: 2048
    guest_port: 10701
    launch_seconds: 45
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := cfg.Backends.DarwinVZ.KernelImage, "/tmp/legacy-kernel"; got != want {
		t.Fatalf("unexpected darwin-vz kernel: got %q want %q", got, want)
	}
	if got, want := cfg.Backends.DarwinVZ.VCPUs, int64(4); got != want {
		t.Fatalf("unexpected darwin-vz vcpus: got %d want %d", got, want)
	}
}

func TestLoadDefaultsBackendWhenMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `backends:
  firecracker:
    binary_path: firecracker
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := cfg.DefaultBackend, DefaultBackendForHost(); got != want {
		t.Fatalf("unexpected default backend: got %q want %q", got, want)
	}
}

func TestLoadTrimsControlHost(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: darwin-vz
control_host: "  unix:///tmp/cleanroom.sock  "
backends:
  firecracker:
    binary_path: firecracker
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := cfg.ControlHost, "unix:///tmp/cleanroom.sock"; got != want {
		t.Fatalf("unexpected control host: got %q want %q", got, want)
	}
}

func TestPathFallsBackToRootHomeWhenHomeUnavailable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")

	prevUserHome := runtimeconfigUserHomeDir
	prevEUID := runtimeconfigGeteuid
	prevGOOS := runtimeconfigGOOS
	runtimeconfigUserHomeDir = func() (string, error) {
		return "", errors.New("$HOME is not defined")
	}
	runtimeconfigGeteuid = func() int { return 0 }
	runtimeconfigGOOS = "darwin"
	t.Cleanup(func() {
		runtimeconfigUserHomeDir = prevUserHome
		runtimeconfigGeteuid = prevEUID
		runtimeconfigGOOS = prevGOOS
	})

	path, err := Path()
	if err != nil {
		t.Fatalf("Path returned error: %v", err)
	}
	if got, want := path, filepath.Join("/var/root", ".config", "cleanroom", "config.yaml"); got != want {
		t.Fatalf("unexpected fallback config path: got %q want %q", got, want)
	}
}

func TestPathReturnsErrorWhenHomeUnavailableForNonRoot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")

	prevUserHome := runtimeconfigUserHomeDir
	prevEUID := runtimeconfigGeteuid
	prevGOOS := runtimeconfigGOOS
	runtimeconfigUserHomeDir = func() (string, error) {
		return "", errors.New("$HOME is not defined")
	}
	runtimeconfigGeteuid = func() int { return 1000 }
	runtimeconfigGOOS = "darwin"
	t.Cleanup(func() {
		runtimeconfigUserHomeDir = prevUserHome
		runtimeconfigGeteuid = prevEUID
		runtimeconfigGOOS = prevGOOS
	})

	_, err := Path()
	if err == nil {
		t.Fatal("expected Path to fail when home is unavailable for non-root")
	}
	if !strings.Contains(err.Error(), "$HOME is not defined") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadParsesSnapshotConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `backends:
  firecracker:
    snapshots:
      enabled: true
      driver: file
      base_dir: /var/tmp/cleanroom-snapshots
      quiesce_timeout_seconds: 15
  darwin-vz:
    snapshots:
      enabled: false
      driver: apfs
      base_dir: /var/tmp/cleanroom-darwin
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cfg.Backends.Firecracker.Snapshots.Enabled {
		t.Fatal("expected firecracker snapshots to be enabled")
	}
	if got, want := cfg.Backends.Firecracker.Snapshots.Driver, "file"; got != want {
		t.Fatalf("unexpected firecracker snapshot driver: got %q want %q", got, want)
	}
	if got, want := cfg.Backends.Firecracker.Snapshots.BaseDir, "/var/tmp/cleanroom-snapshots"; got != want {
		t.Fatalf("unexpected firecracker snapshot base_dir: got %q want %q", got, want)
	}
	if got, want := cfg.Backends.Firecracker.Snapshots.QuiesceTimeoutSeconds, int64(15); got != want {
		t.Fatalf("unexpected firecracker snapshot quiesce timeout: got %d want %d", got, want)
	}
	if got, want := cfg.Backends.DarwinVZ.Snapshots.Driver, "apfs"; got != want {
		t.Fatalf("unexpected darwin-vz snapshot driver: got %q want %q", got, want)
	}
	if got, want := cfg.Backends.DarwinVZ.Snapshots.BaseDir, "/var/tmp/cleanroom-darwin"; got != want {
		t.Fatalf("unexpected darwin-vz snapshot base_dir: got %q want %q", got, want)
	}
}

func TestSnapshotConfigForBackend(t *testing.T) {
	cfg := Config{
		Backends: Backends{
			Firecracker: FirecrackerConfig{
				Snapshots: SnapshotConfig{Enabled: true, Driver: "file"},
			},
			DarwinVZ: DarwinVZConfig{
				Snapshots: SnapshotConfig{Enabled: false, Driver: "apfs"},
			},
		},
	}

	firecrackerCfg, ok := SnapshotConfigForBackend(cfg, "firecracker")
	if !ok {
		t.Fatal("expected firecracker snapshot config to resolve")
	}
	if got, want := firecrackerCfg.Driver, "file"; got != want {
		t.Fatalf("unexpected firecracker snapshot driver: got %q want %q", got, want)
	}

	darwinCfg, ok := SnapshotConfigForBackend(cfg, "darwin-vz")
	if !ok {
		t.Fatal("expected darwin-vz snapshot config to resolve")
	}
	if got, want := darwinCfg.Driver, "apfs"; got != want {
		t.Fatalf("unexpected darwin-vz snapshot driver: got %q want %q", got, want)
	}

	if _, ok := SnapshotConfigForBackend(cfg, "unknown"); ok {
		t.Fatal("expected unknown backend lookup to fail")
	}
}

func TestSnapshotDriverOrDefault(t *testing.T) {
	if got, want := SnapshotDriverOrDefault(""), "file"; got != want {
		t.Fatalf("unexpected default snapshot driver: got %q want %q", got, want)
	}
	if got, want := SnapshotDriverOrDefault(" file "), "file"; got != want {
		t.Fatalf("unexpected trimmed snapshot driver: got %q want %q", got, want)
	}
}
