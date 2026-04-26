package runtimeconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/bytesize"
	"github.com/buildkite/cleanroom/internal/endpoint"
	"gopkg.in/yaml.v3"
)

type Config struct {
	DefaultBackend string              `yaml:"default_backend"`
	ControlHost    string              `yaml:"control_host,omitempty"`
	Gateway        GatewayConfig       `yaml:"gateway,omitempty"`
	Agents         map[string]Agent    `yaml:"agents,omitempty"`
	Observability  ObservabilityConfig `yaml:"observability,omitempty"`
	Backends       Backends            `yaml:"backends"`
}

type Agent struct {
	Command     string            `yaml:"command,omitempty"`
	Test        string            `yaml:"test,omitempty"`
	Install     string            `yaml:"install,omitempty"`
	Credentials []AgentCredential `yaml:"credentials,omitempty"`
}

type AgentCredential struct {
	Source string `yaml:"source,omitempty"`
	Target string `yaml:"target,omitempty"`
}

type GatewayConfig struct {
	Git GatewayGitConfig `yaml:"git,omitempty"`
	OCI GatewayOCIConfig `yaml:"oci,omitempty"`
}

type GatewayGitConfig struct {
	CacheHosts []string `yaml:"cache_hosts,omitempty"`
}

type GatewayOCIConfig struct {
	Registries map[string]string `yaml:"registries,omitempty"`
}

type ObservabilityConfig struct {
	Enabled               bool        `yaml:"enabled,omitempty"`
	DeploymentEnvironment string      `yaml:"deployment_environment,omitempty"`
	Logs                  LogConfig   `yaml:"logs,omitempty"`
	OTLP                  OTLPConfig  `yaml:"otlp,omitempty"`
	Traces                TraceConfig `yaml:"traces,omitempty"`
}

type LogConfig struct {
	Format string `yaml:"format,omitempty"`
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

type configFile struct {
	DefaultBackend string              `yaml:"default_backend"`
	ControlHost    string              `yaml:"control_host,omitempty"`
	Gateway        GatewayConfig       `yaml:"gateway,omitempty"`
	Agents         map[string]Agent    `yaml:"agents,omitempty"`
	Observability  ObservabilityConfig `yaml:"observability,omitempty"`
	Backends       backendsFile        `yaml:"backends"`
}

type backendsFile struct {
	Firecracker    FirecrackerConfig `yaml:"firecracker"`
	DarwinVZ       DarwinVZConfig    `yaml:"darwin-vz"`
	DarwinVZLegacy DarwinVZConfig    `yaml:"darwin_vz"`
}

func (f configFile) config() Config {
	cfg := Config{
		DefaultBackend: f.DefaultBackend,
		ControlHost:    f.ControlHost,
		Gateway:        f.Gateway,
		Agents:         f.Agents,
		Observability:  f.Observability,
		Backends: Backends{
			Firecracker: f.Backends.Firecracker,
			DarwinVZ:    f.Backends.DarwinVZ,
		},
	}
	return cfg
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

type ByteSize = bytesize.Size

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
	rawCfg := configFile{}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&rawCfg); err != nil {
		if !errors.Is(err, io.EOF) {
			return Config{}, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	cfg := rawCfg.config()

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
		if darwinVZConfigHasValues(rawCfg.Backends.DarwinVZLegacy) {
			cfg.Backends.DarwinVZ = rawCfg.Backends.DarwinVZLegacy
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
	cfg.Gateway.OCI.Registries = trimStringMap(cfg.Gateway.OCI.Registries)
	cfg.Agents = normalizeAgents(cfg.Agents)
	cfg.Observability.DeploymentEnvironment = strings.TrimSpace(cfg.Observability.DeploymentEnvironment)
	cfg.Observability.Logs.Format = strings.ToLower(strings.TrimSpace(cfg.Observability.Logs.Format))
	if cfg.Observability.Logs.Format == "" {
		cfg.Observability.Logs.Format = "text"
	}
	cfg.Observability.OTLP.Endpoint = strings.TrimSpace(cfg.Observability.OTLP.Endpoint)
	cfg.Observability.OTLP.Protocol = strings.TrimSpace(cfg.Observability.OTLP.Protocol)
	cfg.Observability.OTLP.Headers = trimStringMap(cfg.Observability.OTLP.Headers)
	cfg.Observability.Traces.Exporter = strings.TrimSpace(cfg.Observability.Traces.Exporter)
	cfg.Observability.Traces.Sampling.Mode = strings.TrimSpace(cfg.Observability.Traces.Sampling.Mode)
	cfg.Observability.Traces.URLTemplate = strings.TrimSpace(cfg.Observability.Traces.URLTemplate)
	return cfg
}

func normalizeAgents(in map[string]Agent) map[string]Agent {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]Agent, len(in))
	for name, agent := range in {
		trimmedName := strings.TrimSpace(name)
		if trimmedName == "" {
			continue
		}
		agent.Command = strings.TrimSpace(agent.Command)
		agent.Test = strings.TrimSpace(agent.Test)
		agent.Install = strings.TrimSpace(agent.Install)
		agent.Credentials = normalizeAgentCredentials(agent.Credentials)
		out[trimmedName] = agent
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeAgentCredentials(in []AgentCredential) []AgentCredential {
	if len(in) == 0 {
		return nil
	}
	out := make([]AgentCredential, 0, len(in))
	for _, credential := range in {
		credential.Source = strings.TrimSpace(credential.Source)
		credential.Target = strings.TrimSpace(credential.Target)
		if credential.Source == "" {
			continue
		}
		if credential.Target == "" {
			credential.Target = credential.Source
		}
		out = append(out, credential)
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
	if _, err := ResolveObservabilityLogFormat(cfg); err != nil {
		return err
	}
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

func ResolveObservabilityLogFormat(cfg ObservabilityConfig) (string, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Logs.Format)) {
	case "", "text":
		return "text", nil
	case "json":
		return "json", nil
	default:
		return "", fmt.Errorf("unsupported observability.logs.format %q", cfg.Logs.Format)
	}
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
