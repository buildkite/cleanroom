package runtimeconfig

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"unicode"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/endpoint"
	"gopkg.in/yaml.v3"
)

type Config struct {
	DefaultBackend string              `yaml:"default_backend"`
	ControlHost    string              `yaml:"control_host,omitempty"`
	Gateway        GatewayConfig       `yaml:"gateway,omitempty"`
	Observability  ObservabilityConfig `yaml:"observability,omitempty"`
	Backends       Backends            `yaml:"backends"`
}

type GatewayConfig struct {
	Git GatewayGitConfig `yaml:"git,omitempty"`
}

type GatewayGitConfig struct {
	CacheHosts []string `yaml:"cache_hosts,omitempty"`
}

type ObservabilityConfig struct {
	Enabled               bool        `yaml:"enabled,omitempty"`
	DeploymentEnvironment string      `yaml:"deployment_environment,omitempty"`
	OTLP                  OTLPConfig  `yaml:"otlp,omitempty"`
	Traces                TraceConfig `yaml:"traces,omitempty"`
}

type OTLPConfig struct {
	Endpoint string            `yaml:"endpoint,omitempty"`
	Protocol string            `yaml:"protocol,omitempty"`
	Insecure bool              `yaml:"insecure,omitempty"`
	Headers  map[string]string `yaml:"headers,omitempty"`
}

type TraceConfig struct {
	Exporter    string              `yaml:"exporter,omitempty"`
	Sampling    TraceSamplingConfig `yaml:"sampling,omitempty"`
	URLTemplate string              `yaml:"url_template,omitempty"`
}

type TraceSamplingConfig struct {
	Mode  string   `yaml:"mode,omitempty"`
	Ratio *float64 `yaml:"ratio,omitempty"`
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
	Mode                       string `yaml:"mode,omitempty"`
	Subnet                     string `yaml:"subnet,omitempty"`
	ExternalInterface          string `yaml:"external_interface,omitempty"`
	DisableNAT44               bool   `yaml:"disable_nat44,omitempty"`
	DisableNAT66               bool   `yaml:"disable_nat66,omitempty"`
	DisableDNSProxy            bool   `yaml:"disable_dns_proxy,omitempty"`
	DisableRouterAdvertisement bool   `yaml:"disable_router_advertisement,omitempty"`
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
		out.DarwinVZNetworkExternalInterface = cfg.Backends.DarwinVZ.Network.ExternalInterface
		out.DarwinVZNetworkDisableNAT44 = cfg.Backends.DarwinVZ.Network.DisableNAT44
		out.DarwinVZNetworkDisableNAT66 = cfg.Backends.DarwinVZ.Network.DisableNAT66
		out.DarwinVZNetworkDisableDNSProxy = cfg.Backends.DarwinVZ.Network.DisableDNSProxy
		out.DarwinVZNetworkDisableRouterAdvertisement = cfg.Backends.DarwinVZ.Network.DisableRouterAdvertisement
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

	if !strings.Contains(numberPart, ".") {
		numberValue, err := strconv.ParseInt(numberPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse numeric value: %w", err)
		}
		if numberValue < 0 {
			return 0, errors.New("value must be non-negative")
		}
		if multiplier != 0 && numberValue > math.MaxInt64/multiplier {
			return 0, errors.New("size overflows int64")
		}
		return numberValue * multiplier, nil
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

	return LoadPath(path)
}

func LoadPath(path string) (Config, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Config{}, "", errors.New("runtime config path is empty")
	}

	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, path, nil
		}
		return Config{}, path, fmt.Errorf("read %s: %w", path, err)
	}

	cfg, err := parseConfig(path, b)
	if err != nil {
		return Config{}, path, err
	}
	return cfg, path, nil
}

func parseConfig(path string, raw []byte) (Config, error) {
	cfg := Config{}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	backendPresence := struct {
		Backends struct {
			Firecracker    *yaml.Node `yaml:"firecracker"`
			DarwinVZ       *yaml.Node `yaml:"darwin-vz"`
			LegacyDarwinVZ *yaml.Node `yaml:"darwin_vz"`
		} `yaml:"backends"`
	}{}
	if err := yaml.Unmarshal(raw, &backendPresence); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	presenceCfg := struct {
		Backends struct {
			DarwinVZ struct {
				MinimumRootFSBytes *ByteSize `yaml:"minimum_rootfs_bytes"`
			} `yaml:"darwin-vz"`
		} `yaml:"backends"`
	}{}
	if err := yaml.Unmarshal(raw, &presenceCfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}

	darwinVZMinRootFSBytes := cfg.Backends.DarwinVZ.MinimumRootFSBytes
	darwinVZMinRootFSBytesSet := presenceCfg.Backends.DarwinVZ.MinimumRootFSBytes != nil
	if darwinVZConfigIsZero(cfg.Backends.DarwinVZ) {
		legacyCfg := struct {
			Backends struct {
				DarwinVZ DarwinVZConfig `yaml:"darwin_vz"`
			} `yaml:"backends"`
		}{}
		if err := yaml.Unmarshal(raw, &legacyCfg); err != nil {
			if backendPresence.Backends.LegacyDarwinVZ != nil {
				return Config{}, fmt.Errorf("parse %s: %w", path, err)
			}
		} else if darwinVZConfigHasValues(legacyCfg.Backends.DarwinVZ) {
			cfg.Backends.DarwinVZ = legacyCfg.Backends.DarwinVZ
		}
	}
	if darwinVZMinRootFSBytesSet {
		cfg.Backends.DarwinVZ.MinimumRootFSBytes = darwinVZMinRootFSBytes
	}

	cfg = normalizeConfig(cfg, inferredDefaultBackend(backendPresence.Backends.Firecracker != nil, backendPresence.Backends.DarwinVZ != nil || backendPresence.Backends.LegacyDarwinVZ != nil))
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func normalizeConfig(cfg Config, inferredDefaultBackend string) Config {
	cfg.DefaultBackend = strings.TrimSpace(cfg.DefaultBackend)
	if cfg.DefaultBackend == "" {
		cfg.DefaultBackend = inferredDefaultBackend
	}
	cfg.ControlHost = strings.TrimSpace(cfg.ControlHost)
	cfg.Gateway.Git.CacheHosts = trimStringSlice(cfg.Gateway.Git.CacheHosts)
	cfg.Observability.DeploymentEnvironment = strings.TrimSpace(cfg.Observability.DeploymentEnvironment)
	cfg.Observability.OTLP.Endpoint = strings.TrimSpace(cfg.Observability.OTLP.Endpoint)
	cfg.Observability.OTLP.Protocol = strings.TrimSpace(cfg.Observability.OTLP.Protocol)
	cfg.Observability.OTLP.Headers = trimStringMap(cfg.Observability.OTLP.Headers)
	cfg.Observability.Traces.Exporter = strings.TrimSpace(cfg.Observability.Traces.Exporter)
	cfg.Observability.Traces.Sampling.Mode = strings.TrimSpace(cfg.Observability.Traces.Sampling.Mode)
	cfg.Observability.Traces.URLTemplate = strings.TrimSpace(cfg.Observability.Traces.URLTemplate)
	return cfg
}

func inferredDefaultBackend(hasFirecracker, hasDarwinVZ bool) string {
	if hasFirecracker == hasDarwinVZ {
		return DefaultBackendForHost()
	}
	if hasFirecracker {
		return "firecracker"
	}
	return "darwin-vz"
}

func validateConfig(cfg Config) error {
	switch cfg.DefaultBackend {
	case "firecracker", "darwin-vz":
	default:
		return fmt.Errorf("unsupported default_backend %q (expected firecracker or darwin-vz)", cfg.DefaultBackend)
	}
	if cfg.ControlHost != "" {
		if _, err := endpoint.Resolve(cfg.ControlHost); err != nil {
			return fmt.Errorf("invalid control_host: %w", err)
		}
	}
	if err := validateDarwinVZRuntimeConfig(cfg.Backends.DarwinVZ); err != nil {
		return err
	}
	if err := validateObservabilityConfig(cfg.Observability); err != nil {
		return err
	}
	return nil
}

func validateObservabilityConfig(cfg ObservabilityConfig) error {
	if !cfg.Enabled {
		return nil
	}

	if err := validateTraceExporter(cfg.Traces.Exporter); err != nil {
		return err
	}
	protocol, err := ResolveOTLPTraceProtocol(cfg)
	if err != nil {
		return err
	}
	if err := validateOTLPEndpoint(cfg.OTLP.Endpoint, protocol); err != nil {
		return err
	}

	if err := ValidateTraceSamplingConfig(cfg.Traces.Sampling); err != nil {
		return err
	}
	if err := ValidateTraceURLTemplate(cfg.Traces.URLTemplate); err != nil {
		return err
	}
	return nil
}

func validateTraceExporter(exporter string) error {
	switch strings.ToLower(strings.TrimSpace(exporter)) {
	case "", "otlp":
		return nil
	default:
		return fmt.Errorf("unsupported observability.traces.exporter %q", exporter)
	}
}

// ResolveOTLPTraceProtocol resolves the configured OTLP trace transport
// protocol.
func ResolveOTLPTraceProtocol(cfg ObservabilityConfig) (string, error) {
	if err := validateTraceExporter(cfg.Traces.Exporter); err != nil {
		return "", err
	}
	return normalizeOTLPProtocol(cfg.OTLP.Protocol)
}

// ValidateTraceSamplingConfig validates supported trace sampling modes and
// ratio values.
func ValidateTraceSamplingConfig(cfg TraceSamplingConfig) error {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	ratio := 1.0
	if cfg.Ratio != nil {
		ratio = *cfg.Ratio
	}
	if ratio < 0 || ratio > 1 {
		return fmt.Errorf("observability.traces.sampling.ratio must be between 0 and 1, got %v", ratio)
	}

	switch mode {
	case "", "parentbased_traceidratio", "traceidratio", "always_on", "always_off", "parentbased_always_on", "parentbased_always_off":
		return nil
	default:
		return fmt.Errorf("unsupported observability.traces.sampling.mode %q", cfg.Mode)
	}
}

type TraceURLTemplateData struct {
	TraceID     string
	ExecutionID string
	SandboxID   string
}

func ValidateTraceURLTemplate(templateText string) error {
	if strings.TrimSpace(templateText) == "" {
		return nil
	}
	_, err := executeTraceURLTemplate(templateText, TraceURLTemplateData{
		TraceID:     "0123456789abcdef0123456789abcdef",
		ExecutionID: "execution_example",
		SandboxID:   "sandbox_example",
	})
	if err != nil {
		return fmt.Errorf("invalid observability.traces.url_template: %w", err)
	}
	return nil
}

func RenderTraceURL(cfg ObservabilityConfig, traceID, executionID, sandboxID string) (string, error) {
	if strings.TrimSpace(traceID) == "" {
		return "", nil
	}
	return executeTraceURLTemplate(cfg.Traces.URLTemplate, TraceURLTemplateData{
		TraceID:     strings.TrimSpace(traceID),
		ExecutionID: strings.TrimSpace(executionID),
		SandboxID:   strings.TrimSpace(sandboxID),
	})
}

func executeTraceURLTemplate(templateText string, data TraceURLTemplateData) (string, error) {
	trimmed := strings.TrimSpace(templateText)
	if trimmed == "" {
		return "", nil
	}
	tpl, err := template.New("trace-url").Option("missingkey=error").Parse(trimmed)
	if err != nil {
		return "", err
	}
	var rendered bytes.Buffer
	if err := tpl.Execute(&rendered, map[string]string{
		"TraceID":     data.TraceID,
		"ExecutionID": data.ExecutionID,
		"SandboxID":   data.SandboxID,
	}); err != nil {
		return "", err
	}
	return strings.TrimSpace(rendered.String()), nil
}

func normalizeOTLPProtocol(protocol string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "", "grpc":
		return "grpc", nil
	case "otlp_http", "otlp/http", "http", "http/protobuf":
		return "http/protobuf", nil
	default:
		return "", fmt.Errorf("unsupported observability.otlp.protocol %q", protocol)
	}
}

func validateOTLPEndpoint(endpoint, protocol string) error {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return errors.New("missing observability.otlp.endpoint")
	}
	if !strings.Contains(trimmed, "://") {
		if strings.Contains(trimmed, "/") {
			return fmt.Errorf("invalid observability.otlp.endpoint %q", endpoint)
		}
		return nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("invalid observability.otlp.endpoint %q: %w", endpoint, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("invalid observability.otlp.endpoint %q", endpoint)
	}
	if protocol == "http/protobuf" && parsed.Path != "" && parsed.Path != "/" {
		return fmt.Errorf("observability.otlp.endpoint %q must not include a path when observability.otlp.protocol is http/protobuf", endpoint)
	}
	return nil
}

func validateDarwinVZRuntimeConfig(cfg DarwinVZConfig) error {
	mode := strings.ToLower(strings.TrimSpace(cfg.Network.Mode))
	if mode == "" {
		mode = "filehandle"
	}
	if mode != "filehandle" {
		return fmt.Errorf("unsupported darwin-vz network mode %q: only %q is supported", cfg.Network.Mode, "filehandle")
	}

	removedSettings := make([]string, 0, 5)
	if strings.TrimSpace(cfg.Network.ExternalInterface) != "" {
		removedSettings = append(removedSettings, "external_interface")
	}
	if cfg.Network.DisableNAT44 {
		removedSettings = append(removedSettings, "disable_nat44")
	}
	if cfg.Network.DisableNAT66 {
		removedSettings = append(removedSettings, "disable_nat66")
	}
	if cfg.Network.DisableDNSProxy {
		removedSettings = append(removedSettings, "disable_dns_proxy")
	}
	if cfg.Network.DisableRouterAdvertisement {
		removedSettings = append(removedSettings, "disable_router_advertisement")
	}
	if len(removedSettings) == 0 {
		return nil
	}
	return fmt.Errorf("darwin-vz no longer supports legacy vmnet settings: %s", strings.Join(removedSettings, ", "))
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

func trimStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}

	out := make(map[string]string, len(input))
	for key, value := range input {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		out[trimmedKey] = strings.TrimSpace(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func trimStringSlice(input []string) []string {
	if len(input) == 0 {
		return nil
	}

	out := make([]string, 0, len(input))
	for _, value := range input {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
