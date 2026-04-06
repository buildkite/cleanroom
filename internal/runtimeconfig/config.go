package runtimeconfig

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"

	"github.com/buildkite/cleanroom/internal/backend"
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
	PrivilegedHelperPath string         `yaml:"privileged_helper_path"`
	VCPUs                int64          `yaml:"vcpus"`
	MemoryMiB            int64          `yaml:"memory_mib"`
	GuestCID             uint32         `yaml:"guest_cid"`
	GuestPort            uint32         `yaml:"guest_port"`
	LaunchSeconds        int64          `yaml:"launch_seconds"` // VM boot/guest-agent readiness timeout
}

type DarwinVZConfig struct {
	KernelImage        string                `yaml:"kernel_image"`
	RootFS             string                `yaml:"rootfs"`
	MinimumRootFSBytes ByteSize              `yaml:"minimum_rootfs_bytes"`
	Network            DarwinVZNetworkConfig `yaml:"network,omitempty"`
	Services           ServicesConfig        `yaml:"services"`
	Snapshots          SnapshotConfig        `yaml:"snapshots"`
	VCPUs              int64                 `yaml:"vcpus"`
	MemoryMiB          int64                 `yaml:"memory_mib"`
	GuestPort          uint32                `yaml:"guest_port"`
	LaunchSeconds      int64                 `yaml:"launch_seconds"` // VM boot/guest-agent readiness timeout
}

type DarwinVZNetworkConfig struct {
	Mode   string `yaml:"mode,omitempty"`
	Subnet string `yaml:"subnet,omitempty"`
}

type SnapshotConfig struct {
	Enabled               bool   `yaml:"enabled"`
	Driver                string `yaml:"driver"`
	BaseDir               string `yaml:"base_dir,omitempty"`
	ZFSDataset            string `yaml:"zfs_dataset,omitempty"`
	QuiesceTimeoutSeconds int64  `yaml:"quiesce_timeout_seconds,omitempty"`
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

func SnapshotDriverOrDefault(backendName, driver string) string {
	driver = strings.TrimSpace(driver)
	if driver != "" {
		return driver
	}

	switch strings.TrimSpace(backendName) {
	case "darwin-vz":
		return "apfs"
	default:
		return "file"
	}
}

func MergeBackendConfig(cfg Config, backendName string, launchSeconds int64) backend.FirecrackerConfig {
	out := backend.FirecrackerConfig{
		BinaryPath:           cfg.Backends.Firecracker.BinaryPath,
		KernelImagePath:      cfg.Backends.Firecracker.KernelImage,
		RootFSPath:           cfg.Backends.Firecracker.RootFS,
		DockerStartupSeconds: cfg.Backends.Firecracker.Services.Docker.StartupTimeoutSeconds,
		DockerStorageDriver:  cfg.Backends.Firecracker.Services.Docker.StorageDriver,
		DockerIPTables:       cfg.Backends.Firecracker.Services.Docker.IPTables,
		Snapshots: backend.SnapshotConfig{
			Enabled:               cfg.Backends.Firecracker.Snapshots.Enabled,
			Driver:                cfg.Backends.Firecracker.Snapshots.Driver,
			BaseDir:               cfg.Backends.Firecracker.Snapshots.BaseDir,
			ZFSDataset:            cfg.Backends.Firecracker.Snapshots.ZFSDataset,
			QuiesceTimeoutSeconds: cfg.Backends.Firecracker.Snapshots.QuiesceTimeoutSeconds,
		},
		PrivilegedHelperPath: cfg.Backends.Firecracker.PrivilegedHelperPath,
		VCPUs:                cfg.Backends.Firecracker.VCPUs,
		MemoryMiB:            cfg.Backends.Firecracker.MemoryMiB,
		GuestCID:             cfg.Backends.Firecracker.GuestCID,
		GuestPort:            cfg.Backends.Firecracker.GuestPort,
		LaunchSeconds:        cfg.Backends.Firecracker.LaunchSeconds,
	}
	if backendName == "darwin-vz" {
		out.KernelImagePath = cfg.Backends.DarwinVZ.KernelImage
		out.RootFSPath = cfg.Backends.DarwinVZ.RootFS
		out.MinimumRootFSBytes = int64(cfg.Backends.DarwinVZ.MinimumRootFSBytes)
		out.DarwinVZNetworkMode = cfg.Backends.DarwinVZ.Network.Mode
		out.DarwinVZNetworkSubnet = cfg.Backends.DarwinVZ.Network.Subnet
		out.DockerStartupSeconds = cfg.Backends.DarwinVZ.Services.Docker.StartupTimeoutSeconds
		out.DockerStorageDriver = cfg.Backends.DarwinVZ.Services.Docker.StorageDriver
		out.DockerIPTables = cfg.Backends.DarwinVZ.Services.Docker.IPTables
		out.Snapshots = backend.SnapshotConfig{
			Enabled:               cfg.Backends.DarwinVZ.Snapshots.Enabled,
			Driver:                cfg.Backends.DarwinVZ.Snapshots.Driver,
			BaseDir:               cfg.Backends.DarwinVZ.Snapshots.BaseDir,
			ZFSDataset:            cfg.Backends.DarwinVZ.Snapshots.ZFSDataset,
			QuiesceTimeoutSeconds: cfg.Backends.DarwinVZ.Snapshots.QuiesceTimeoutSeconds,
		}
		out.VCPUs = cfg.Backends.DarwinVZ.VCPUs
		out.MemoryMiB = cfg.Backends.DarwinVZ.MemoryMiB
		out.GuestPort = cfg.Backends.DarwinVZ.GuestPort
		out.LaunchSeconds = cfg.Backends.DarwinVZ.LaunchSeconds
	}

	out.Launch = true
	if launchSeconds != 0 {
		out.LaunchSeconds = launchSeconds
	}
	return out
}

type ServicesConfig struct {
	Docker DockerServiceConfig `yaml:"docker"`
}

type DockerServiceConfig struct {
	StartupTimeoutSeconds int64  `yaml:"startup_timeout_seconds"`
	StorageDriver         string `yaml:"storage_driver"`
	IPTables              bool   `yaml:"iptables"`
}

type ByteSize int64

func (s *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("invalid byte size node kind %v", node.Kind)
	}

	value, err := parseByteSize(node.Value)
	if err != nil {
		return fmt.Errorf("invalid byte size %q: %w", node.Value, err)
	}
	*s = ByteSize(value)
	return nil
}

func parseByteSize(input string) (int64, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return 0, errors.New("value is empty")
	}

	if value, err := strconv.ParseInt(s, 10, 64); err == nil {
		if value < 0 {
			return 0, errors.New("value must be non-negative")
		}
		return value, nil
	}

	numberEnd := strings.IndexFunc(s, func(r rune) bool {
		return !(unicode.IsDigit(r) || r == '.')
	})
	if numberEnd <= 0 {
		return 0, errors.New("missing numeric value")
	}

	numberPart := strings.TrimSpace(s[:numberEnd])
	unitPart := strings.ToLower(strings.TrimSpace(s[numberEnd:]))
	if numberPart == "" || unitPart == "" {
		return 0, errors.New("size must include a number and unit")
	}

	numberValue, err := strconv.ParseFloat(numberPart, 64)
	if err != nil {
		return 0, fmt.Errorf("parse numeric value: %w", err)
	}
	if numberValue < 0 {
		return 0, errors.New("value must be non-negative")
	}

	multiplier, ok := byteSizeMultipliers[unitPart]
	if !ok {
		return 0, fmt.Errorf("unsupported unit %q", unitPart)
	}

	value := numberValue * float64(multiplier)
	rounded := math.Round(value)
	if math.Abs(value-rounded) > 1e-9 {
		return 0, errors.New("size resolves to fractional bytes")
	}
	if rounded > math.MaxInt64 {
		return 0, errors.New("size overflows int64")
	}
	return int64(rounded), nil
}

var byteSizeMultipliers = map[string]int64{
	"b":   1,
	"k":   1 << 10,
	"kb":  1000,
	"kib": 1 << 10,
	"m":   1 << 20,
	"mb":  1000 * 1000,
	"mib": 1 << 20,
	"g":   1 << 30,
	"gb":  1000 * 1000 * 1000,
	"gib": 1 << 30,
	"t":   1 << 40,
	"tb":  1000 * 1000 * 1000 * 1000,
	"tib": 1 << 40,
	"p":   1 << 50,
	"pb":  1000 * 1000 * 1000 * 1000 * 1000,
	"pib": 1 << 50,
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
	presenceCfg := struct {
		Backends struct {
			DarwinVZ struct {
				MinimumRootFSBytes *ByteSize `yaml:"minimum_rootfs_bytes"`
			} `yaml:"darwin-vz"`
			LegacyDarwinVZ *yaml.Node `yaml:"darwin_vz"`
		} `yaml:"backends"`
	}{}
	if err := yaml.Unmarshal(b, &presenceCfg); err != nil {
		return Config{}, path, fmt.Errorf("parse %s: %w", path, err)
	}

	darwinVZMinRootFSBytes := cfg.Backends.DarwinVZ.MinimumRootFSBytes
	darwinVZMinRootFSBytesSet := presenceCfg.Backends.DarwinVZ.MinimumRootFSBytes != nil
	if darwinVZConfigIsZero(cfg.Backends.DarwinVZ) {
		legacyCfg := struct {
			Backends struct {
				DarwinVZ DarwinVZConfig `yaml:"darwin_vz"`
			} `yaml:"backends"`
		}{}
		if err := yaml.Unmarshal(b, &legacyCfg); err != nil {
			if presenceCfg.Backends.LegacyDarwinVZ != nil {
				return Config{}, path, fmt.Errorf("parse %s: %w", path, err)
			}
		} else if darwinVZConfigHasValues(legacyCfg.Backends.DarwinVZ) {
			cfg.Backends.DarwinVZ = legacyCfg.Backends.DarwinVZ
		}
	}
	if darwinVZMinRootFSBytesSet {
		cfg.Backends.DarwinVZ.MinimumRootFSBytes = darwinVZMinRootFSBytes
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
		darwinVZNetworkConfigIsZero(cfg.Network) &&
		cfg.Services.Docker.StartupTimeoutSeconds == 0 &&
		strings.TrimSpace(cfg.Services.Docker.StorageDriver) == "" &&
		!cfg.Services.Docker.IPTables &&
		snapshotConfigIsZero(cfg.Snapshots) &&
		cfg.VCPUs == 0 &&
		cfg.MemoryMiB == 0 &&
		cfg.GuestPort == 0 &&
		cfg.LaunchSeconds == 0
}

func darwinVZConfigHasValues(cfg DarwinVZConfig) bool {
	return !darwinVZConfigIsZero(cfg) || cfg.MinimumRootFSBytes > 0
}

func darwinVZNetworkConfigIsZero(cfg DarwinVZNetworkConfig) bool {
	return strings.TrimSpace(cfg.Mode) == "" &&
		strings.TrimSpace(cfg.Subnet) == ""
}

func snapshotConfigIsZero(cfg SnapshotConfig) bool {
	return !cfg.Enabled &&
		strings.TrimSpace(cfg.Driver) == "" &&
		strings.TrimSpace(cfg.BaseDir) == "" &&
		strings.TrimSpace(cfg.ZFSDataset) == "" &&
		cfg.QuiesceTimeoutSeconds == 0
}
