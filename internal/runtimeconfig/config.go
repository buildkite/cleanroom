package runtimeconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	DefaultBackend string   `yaml:"default_backend"`
	ControlHost    string   `yaml:"control_host,omitempty"`
	Backends       Backends `yaml:"backends"`
}

type Backends struct {
	Firecracker FirecrackerConfig `yaml:"firecracker"`
	DarwinVZ    DarwinVZConfig    `yaml:"darwin-vz"`
}

type FirecrackerConfig struct {
	BinaryPath           string         `yaml:"binary_path"`
	KernelImage          string         `yaml:"kernel_image"`
	RootFS               string         `yaml:"rootfs"`
	Services             ServicesConfig `yaml:"services"`
	Snapshots            SnapshotConfig `yaml:"snapshots"`
	PrivilegedMode       string         `yaml:"privileged_mode"`
	PrivilegedHelperPath string         `yaml:"privileged_helper_path"`
	VCPUs                int64          `yaml:"vcpus"`
	MemoryMiB            int64          `yaml:"memory_mib"`
	GuestCID             uint32         `yaml:"guest_cid"`
	GuestPort            uint32         `yaml:"guest_port"`
	LaunchSeconds        int64          `yaml:"launch_seconds"` // VM boot/guest-agent readiness timeout
}

type DarwinVZConfig struct {
	KernelImage   string         `yaml:"kernel_image"`
	RootFS        string         `yaml:"rootfs"`
	Services      ServicesConfig `yaml:"services"`
	Snapshots     SnapshotConfig `yaml:"snapshots"`
	VCPUs         int64          `yaml:"vcpus"`
	MemoryMiB     int64          `yaml:"memory_mib"`
	GuestPort     uint32         `yaml:"guest_port"`
	LaunchSeconds int64          `yaml:"launch_seconds"` // VM boot/guest-agent readiness timeout
}

type SnapshotConfig struct {
	Enabled               bool   `yaml:"enabled"`
	Driver                string `yaml:"driver"`
	BaseDir               string `yaml:"base_dir"`
	QuiesceTimeoutSeconds int64  `yaml:"quiesce_timeout_seconds"`
}

func SnapshotConfigForBackend(cfg Config, backendName string) (SnapshotConfig, bool) {
	switch strings.TrimSpace(backendName) {
	case "firecracker":
		return cfg.Backends.Firecracker.Snapshots, true
	case "darwin-vz":
		return cfg.Backends.DarwinVZ.Snapshots, true
	default:
		return SnapshotConfig{}, false
	}
}

func SnapshotDriverOrDefault(driver string) string {
	driver = strings.TrimSpace(driver)
	if driver == "" {
		return "file"
	}
	return driver
}

type ServicesConfig struct {
	Docker DockerServiceConfig `yaml:"docker"`
}

type DockerServiceConfig struct {
	StartupTimeoutSeconds int64  `yaml:"startup_timeout_seconds"`
	StorageDriver         string `yaml:"storage_driver"`
	IPTables              bool   `yaml:"iptables"`
}

var (
	runtimeconfigUserHomeDir = os.UserHomeDir
	runtimeconfigGeteuid     = os.Geteuid
	runtimeconfigGOOS        = runtime.GOOS
)

func DefaultBackendForGOOS(goos string) string {
	if strings.EqualFold(strings.TrimSpace(goos), "darwin") {
		return "darwin-vz"
	}
	return "firecracker"
}

func DefaultBackendForHost() string {
	return DefaultBackendForGOOS(runtime.GOOS)
}

func Path() (string, error) {
	configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configHome != "" {
		return filepath.Join(configHome, "cleanroom", "config.yaml"), nil
	}

	home, err := runtimeconfigUserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		fallback, fallbackOK := defaultRootHomeDir()
		if !fallbackOK {
			if err != nil {
				return "", err
			}
			return "", errors.New("home directory is not available")
		}
		home = fallback
	}
	return filepath.Join(home, ".config", "cleanroom", "config.yaml"), nil
}

func defaultRootHomeDir() (string, bool) {
	if runtimeconfigGeteuid() != 0 {
		return "", false
	}

	if strings.EqualFold(strings.TrimSpace(runtimeconfigGOOS), "darwin") {
		return "/var/root", true
	}
	if strings.EqualFold(strings.TrimSpace(runtimeconfigGOOS), "windows") {
		return "", false
	}
	return "/root", true
}

func Load() (Config, string, error) {
	path, err := Path()
	if err != nil {
		return Config{}, "", err
	}

	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, path, nil
		}
		return Config{}, path, fmt.Errorf("read %s: %w", path, err)
	}

	cfg := Config{}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, path, fmt.Errorf("parse %s: %w", path, err)
	}
	if darwinVZConfigIsZero(cfg.Backends.DarwinVZ) {
		legacyCfg := struct {
			Backends struct {
				DarwinVZ DarwinVZConfig `yaml:"darwin_vz"`
			} `yaml:"backends"`
		}{}
		if err := yaml.Unmarshal(b, &legacyCfg); err == nil && !darwinVZConfigIsZero(legacyCfg.Backends.DarwinVZ) {
			cfg.Backends.DarwinVZ = legacyCfg.Backends.DarwinVZ
		}
	}

	cfg.DefaultBackend = strings.TrimSpace(cfg.DefaultBackend)
	if cfg.DefaultBackend == "" {
		cfg.DefaultBackend = DefaultBackendForHost()
	}
	cfg.ControlHost = strings.TrimSpace(cfg.ControlHost)
	return cfg, path, nil
}

func darwinVZConfigIsZero(cfg DarwinVZConfig) bool {
	return strings.TrimSpace(cfg.KernelImage) == "" &&
		strings.TrimSpace(cfg.RootFS) == "" &&
		cfg.Services.Docker.StartupTimeoutSeconds == 0 &&
		strings.TrimSpace(cfg.Services.Docker.StorageDriver) == "" &&
		!cfg.Services.Docker.IPTables &&
		snapshotConfigIsZero(cfg.Snapshots) &&
		cfg.VCPUs == 0 &&
		cfg.MemoryMiB == 0 &&
		cfg.GuestPort == 0 &&
		cfg.LaunchSeconds == 0
}

func snapshotConfigIsZero(cfg SnapshotConfig) bool {
	return !cfg.Enabled &&
		strings.TrimSpace(cfg.Driver) == "" &&
		strings.TrimSpace(cfg.BaseDir) == "" &&
		cfg.QuiesceTimeoutSeconds == 0
}
