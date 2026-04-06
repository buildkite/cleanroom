package runtimeconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
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
    network:
      mode: vmnet-shared
      subnet: 10.233.0.0/16
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
	if got, want := cfg.Backends.DarwinVZ.Network.Mode, "vmnet-shared"; got != want {
		t.Fatalf("unexpected darwin-vz network mode: got %q want %q", got, want)
	}
	if got, want := cfg.Backends.DarwinVZ.Network.Subnet, "10.233.0.0/16"; got != want {
		t.Fatalf("unexpected darwin-vz network subnet: got %q want %q", got, want)
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

func TestLoadSupportsDarwinVZMinimumRootFSBytes(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: darwin-vz
backends:
  darwin-vz:
    rootfs: /tmp/rootfs
    minimum_rootfs_bytes: 2147483648
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := int64(cfg.Backends.DarwinVZ.MinimumRootFSBytes), int64(2147483648); got != want {
		t.Fatalf("unexpected darwin-vz minimum rootfs bytes: got %d want %d", got, want)
	}
}

func TestLoadSupportsDarwinVZMinimumRootFSBytesHumanString(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: darwin-vz
backends:
  darwin-vz:
    rootfs: /tmp/rootfs
    minimum_rootfs_bytes: 2GiB
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := int64(cfg.Backends.DarwinVZ.MinimumRootFSBytes), int64(2<<30); got != want {
		t.Fatalf("unexpected darwin-vz minimum rootfs bytes: got %d want %d", got, want)
	}
}

func TestLoadSupportsDarwinVZMinimumRootFSBytesQuotedNumericString(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: darwin-vz
backends:
  darwin-vz:
    rootfs: /tmp/rootfs
    minimum_rootfs_bytes: "2147483648"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := int64(cfg.Backends.DarwinVZ.MinimumRootFSBytes), int64(2147483648); got != want {
		t.Fatalf("unexpected darwin-vz minimum rootfs bytes: got %d want %d", got, want)
	}
}

func TestLoadRejectsInvalidDarwinVZMinimumRootFSBytes(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: darwin-vz
backends:
  darwin-vz:
    rootfs: /tmp/rootfs
    minimum_rootfs_bytes: giant
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected Load to reject invalid minimum_rootfs_bytes")
	}
	if !strings.Contains(err.Error(), "invalid byte size") {
		t.Fatalf("expected error to mention invalid byte size, got %v", err)
	}
}

func TestLoadPreservesLegacyDarwinVZFallbackWhenOnlyMinRootFSIsSet(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: darwin-vz
backends:
  darwin-vz:
    minimum_rootfs_bytes: 2147483648
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
	if got, want := cfg.Backends.DarwinVZ.RootFS, "/tmp/legacy-rootfs"; got != want {
		t.Fatalf("unexpected darwin-vz rootfs: got %q want %q", got, want)
	}
	if got, want := int64(cfg.Backends.DarwinVZ.MinimumRootFSBytes), int64(2147483648); got != want {
		t.Fatalf("unexpected darwin-vz minimum rootfs bytes: got %d want %d", got, want)
	}
}

func TestLoadSupportsLegacyDarwinVZMinimumRootFSBytesOnly(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: darwin-vz
backends:
  darwin_vz:
    minimum_rootfs_bytes: 2GiB
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := int64(cfg.Backends.DarwinVZ.MinimumRootFSBytes), int64(2<<30); got != want {
		t.Fatalf("unexpected darwin-vz minimum rootfs bytes: got %d want %d", got, want)
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
      driver: zfs
      base_dir: /var/tmp/cleanroom-snapshots
      zfs_dataset: tank/cleanroom
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
	if got, want := cfg.Backends.Firecracker.Snapshots.Driver, "zfs"; got != want {
		t.Fatalf("unexpected firecracker snapshot driver: got %q want %q", got, want)
	}
	if got, want := cfg.Backends.Firecracker.Snapshots.BaseDir, "/var/tmp/cleanroom-snapshots"; got != want {
		t.Fatalf("unexpected firecracker snapshot base_dir: got %q want %q", got, want)
	}
	if got, want := cfg.Backends.Firecracker.Snapshots.ZFSDataset, "tank/cleanroom"; got != want {
		t.Fatalf("unexpected firecracker snapshot zfs_dataset: got %q want %q", got, want)
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
	if got, want := SnapshotDriverOrDefault("firecracker", ""), "file"; got != want {
		t.Fatalf("unexpected firecracker default snapshot driver: got %q want %q", got, want)
	}
	if got, want := SnapshotDriverOrDefault("darwin-vz", ""), "apfs"; got != want {
		t.Fatalf("unexpected darwin-vz default snapshot driver: got %q want %q", got, want)
	}
	if got, want := SnapshotDriverOrDefault("darwin-vz", " file "), "file"; got != want {
		t.Fatalf("unexpected trimmed snapshot driver: got %q want %q", got, want)
	}
}

func TestMergeBackendConfig(t *testing.T) {
	cfg := Config{
		Backends: Backends{
			Firecracker: FirecrackerConfig{
				BinaryPath:           "firecracker-bin",
				KernelImage:          "/firecracker/kernel",
				RootFS:               "/firecracker/rootfs.ext4",
				Services:             ServicesConfig{Docker: DockerServiceConfig{StartupTimeoutSeconds: 12, StorageDriver: "overlay2", IPTables: true}},
				Snapshots:            SnapshotConfig{Enabled: true, Driver: "zfs", BaseDir: "/firecracker/snapshots", ZFSDataset: "tank/cleanroom", QuiesceTimeoutSeconds: 15},
				PrivilegedHelperPath: "/usr/local/bin/cleanroom-root-helper",
				VCPUs:                2,
				MemoryMiB:            1024,
				GuestCID:             111,
				GuestPort:            10700,
				LaunchSeconds:        30,
			},
			DarwinVZ: DarwinVZConfig{
				KernelImage:        "/darwin/kernel",
				RootFS:             "/darwin/rootfs.ext4",
				MinimumRootFSBytes: 2147483648,
				Network:            DarwinVZNetworkConfig{Mode: "vmnet-shared", Subnet: "10.233.0.0/16"},
				Services:           ServicesConfig{Docker: DockerServiceConfig{StartupTimeoutSeconds: 20, StorageDriver: "vzfs", IPTables: false}},
				Snapshots:          SnapshotConfig{Enabled: false, Driver: "apfs", BaseDir: "/darwin/snapshots", QuiesceTimeoutSeconds: 22},
				VCPUs:              4,
				MemoryMiB:          2048,
				GuestPort:          10701,
				LaunchSeconds:      45,
			},
		},
	}

	firecrackerCfg := MergeBackendConfig(cfg, "firecracker", 99)
	if !firecrackerCfg.Launch {
		t.Fatal("expected merged firecracker config to enable launch")
	}
	if got, want := firecrackerCfg.LaunchSeconds, int64(99); got != want {
		t.Fatalf("unexpected firecracker launch seconds: got %d want %d", got, want)
	}
	if got, want := firecrackerCfg.Snapshots, (backend.SnapshotConfig{Enabled: true, Driver: "zfs", BaseDir: "/firecracker/snapshots", ZFSDataset: "tank/cleanroom", QuiesceTimeoutSeconds: 15}); got != want {
		t.Fatalf("unexpected firecracker snapshots config: got %#v want %#v", got, want)
	}

	darwinCfg := MergeBackendConfig(cfg, "darwin-vz", 0)
	if !darwinCfg.Launch {
		t.Fatal("expected merged darwin-vz config to enable launch")
	}
	if got, want := darwinCfg.KernelImagePath, "/darwin/kernel"; got != want {
		t.Fatalf("unexpected darwin-vz kernel image: got %q want %q", got, want)
	}
	if got, want := darwinCfg.RootFSPath, "/darwin/rootfs.ext4"; got != want {
		t.Fatalf("unexpected darwin-vz rootfs: got %q want %q", got, want)
	}
	if got, want := darwinCfg.MinimumRootFSBytes, int64(2147483648); got != want {
		t.Fatalf("unexpected darwin-vz minimum rootfs bytes: got %d want %d", got, want)
	}
	if got, want := darwinCfg.DarwinVZNetworkMode, "vmnet-shared"; got != want {
		t.Fatalf("unexpected darwin-vz network mode: got %q want %q", got, want)
	}
	if got, want := darwinCfg.DarwinVZNetworkSubnet, "10.233.0.0/16"; got != want {
		t.Fatalf("unexpected darwin-vz network subnet: got %q want %q", got, want)
	}
	if got, want := darwinCfg.DockerStorageDriver, "vzfs"; got != want {
		t.Fatalf("unexpected darwin-vz docker storage driver: got %q want %q", got, want)
	}
	if got, want := darwinCfg.Snapshots, (backend.SnapshotConfig{Enabled: false, Driver: "apfs", BaseDir: "/darwin/snapshots", QuiesceTimeoutSeconds: 22}); got != want {
		t.Fatalf("unexpected darwin-vz snapshots config: got %#v want %#v", got, want)
	}
	if got, want := darwinCfg.VCPUs, int64(4); got != want {
		t.Fatalf("unexpected darwin-vz vcpus: got %d want %d", got, want)
	}
	if got, want := darwinCfg.MemoryMiB, int64(2048); got != want {
		t.Fatalf("unexpected darwin-vz memory: got %d want %d", got, want)
	}
	if got, want := darwinCfg.GuestPort, uint32(10701); got != want {
		t.Fatalf("unexpected darwin-vz guest port: got %d want %d", got, want)
	}
	if got, want := darwinCfg.LaunchSeconds, int64(45); got != want {
		t.Fatalf("unexpected darwin-vz launch seconds: got %d want %d", got, want)
	}
	if got, want := darwinCfg.BinaryPath, "firecracker-bin"; got != want {
		t.Fatalf("unexpected retained binary path: got %q want %q", got, want)
	}
}
