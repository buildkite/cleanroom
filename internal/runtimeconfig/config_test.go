package runtimeconfig

import (
	"os"
	"path/filepath"
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
