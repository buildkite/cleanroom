package runtimeconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/bytesize"
	"github.com/buildkite/cleanroom/internal/endpoint"
	"gopkg.in/yaml.v3"
)

type Config struct {
	DefaultBackend   string                 `yaml:"default_backend"`
	ControlHost      string                 `yaml:"control_host,omitempty"`
	Auth             AuthConfig             `yaml:"auth,omitempty"`
	Cache            CacheConfig            `yaml:"cache,omitempty"`
	Gateway          GatewayConfig          `yaml:"gateway,omitempty"`
	Observability    ObservabilityConfig    `yaml:"observability,omitempty"`
	SandboxLifecycle SandboxLifecycleConfig `yaml:"sandbox_lifecycle,omitempty"`
	Backends         Backends               `yaml:"backends"`
}

const (
	DefaultAuthOIDCClockSkewSeconds        int64 = 60
	DefaultAuthOIDCMaxTokenLifetimeSeconds int64 = 3600
)

// AuthConfig configures control-plane caller authentication.
type AuthConfig struct {
	Required   bool           `yaml:"required,omitempty"`
	OIDC       AuthOIDCConfig `yaml:"oidc,omitempty"`
	PolicyFile string         `yaml:"policy_file,omitempty"`
}

type AuthOIDCConfig struct {
	Issuers []AuthOIDCIssuerConfig `yaml:"issuers,omitempty"`
}

type AuthOIDCIssuerConfig struct {
	Name                    string   `yaml:"name,omitempty"`
	Issuer                  string   `yaml:"issuer"`
	Audiences               []string `yaml:"audiences,omitempty"`
	JWKSURL                 string   `yaml:"jwks_url,omitempty"`
	AllowedAlgorithms       []string `yaml:"allowed_algorithms,omitempty"`
	ClockSkewSeconds        int64    `yaml:"clock_skew_seconds,omitempty"`
	MaxTokenLifetimeSeconds int64    `yaml:"max_token_lifetime_seconds,omitempty"`
}

// CacheConfig configures host-to-host cache reuse.
type CacheConfig struct {
	Peers []CachePeerConfig `yaml:"peers,omitempty"`
}

// CachePeerConfig identifies a trusted Cleanroom cache peer.
type CachePeerConfig struct {
	URL      string `yaml:"url"`
	TokenEnv string `yaml:"token_env,omitempty"`
}

type SandboxLifecycleConfig struct {
	IdleSuspendAfterSeconds int64 `yaml:"idle_suspend_after_seconds,omitempty"`
	WakeTimeoutSeconds      int64 `yaml:"wake_timeout_seconds,omitempty"`
}

type GatewayConfig struct {
	Git         GatewayGitConfig         `yaml:"git,omitempty"`
	OCI         GatewayOCIConfig         `yaml:"oci,omitempty"`
	Credentials GatewayCredentialsConfig `yaml:"credentials,omitempty"`
}

type GatewayGitConfig struct {
	CacheHosts []string `yaml:"cache_hosts,omitempty"`
}

type GatewayOCIConfig struct {
	Registries map[string]string `yaml:"registries,omitempty"`
}

type GatewayCredentialsConfig struct {
	GitHubApp GatewayGitHubAppCredentialsConfig `yaml:"github_app,omitempty"`
}

type GatewayGitHubAppCredentialsConfig struct {
	AppID          ScalarString `yaml:"app_id,omitempty"`
	InstallationID ScalarString `yaml:"installation_id,omitempty"`
	PrivateKeyFile string       `yaml:"private_key_file,omitempty"`
	RepoPrefixes   []string     `yaml:"repo_prefixes,omitempty"`
}

// ScalarString preserves scalar config values as trimmed strings while allowing
// natural YAML numeric forms for IDs.
type ScalarString string

func (s *ScalarString) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("expected scalar value, got %v", node.Kind)
	}
	*s = ScalarString(strings.TrimSpace(node.Value))
	return nil
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
	DefaultBackend   string                 `yaml:"default_backend"`
	ControlHost      string                 `yaml:"control_host,omitempty"`
	Auth             AuthConfig             `yaml:"auth,omitempty"`
	Cache            CacheConfig            `yaml:"cache,omitempty"`
	Gateway          GatewayConfig          `yaml:"gateway,omitempty"`
	Observability    ObservabilityConfig    `yaml:"observability,omitempty"`
	SandboxLifecycle SandboxLifecycleConfig `yaml:"sandbox_lifecycle,omitempty"`
	Backends         backendsFile           `yaml:"backends"`
}

type backendsFile struct {
	Firecracker    FirecrackerConfig `yaml:"firecracker"`
	DarwinVZ       DarwinVZConfig    `yaml:"darwin-vz"`
	DarwinVZLegacy DarwinVZConfig    `yaml:"darwin_vz"`
}

func (f configFile) config() Config {
	cfg := Config{
		DefaultBackend:   f.DefaultBackend,
		ControlHost:      f.ControlHost,
		Auth:             f.Auth,
		Cache:            f.Cache,
		Gateway:          f.Gateway,
		Observability:    f.Observability,
		SandboxLifecycle: f.SandboxLifecycle,
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
	BinaryPath                    string         `yaml:"binary_path"`
	KernelImage                   string         `yaml:"kernel_image"`
	RootFS                        string         `yaml:"rootfs"`
	MinimumCacheOutputVolumeBytes ByteSize       `yaml:"minimum_cache_output_volume_bytes"`
	Services                      ServicesConfig `yaml:"services"`
	Snapshots                     SnapshotConfig `yaml:"snapshots"`
	PrivilegedHelperPath          string         `yaml:"privileged_helper_path"`
	VCPUs                         int64          `yaml:"vcpus"`
	MemoryMiB                     int64          `yaml:"memory_mib"`
	GuestCID                      uint32         `yaml:"guest_cid"`
	GuestPort                     uint32         `yaml:"guest_port"`
	LaunchSeconds                 int64          `yaml:"launch_seconds"` // VM boot/guest-agent readiness timeout
}

type DarwinVZConfig struct {
	KernelImage                   string                `yaml:"kernel_image"`
	RootFS                        string                `yaml:"rootfs"`
	MinimumRootFSBytes            ByteSize              `yaml:"minimum_rootfs_bytes"`
	MinimumCacheOutputVolumeBytes ByteSize              `yaml:"minimum_cache_output_volume_bytes"`
	Network                       DarwinVZNetworkConfig `yaml:"network,omitempty"`
	Services                      ServicesConfig        `yaml:"services"`
	Snapshots                     SnapshotConfig        `yaml:"snapshots"`
	VCPUs                         int64                 `yaml:"vcpus"`
	MemoryMiB                     int64                 `yaml:"memory_mib"`
	GuestPort                     uint32                `yaml:"guest_port"`
	LaunchSeconds                 int64                 `yaml:"launch_seconds"` // VM boot/guest-agent readiness timeout
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
		PrivilegedHelperPath:          cfg.Backends.Firecracker.PrivilegedHelperPath,
		VCPUs:                         cfg.Backends.Firecracker.VCPUs,
		MemoryMiB:                     cfg.Backends.Firecracker.MemoryMiB,
		GuestCID:                      cfg.Backends.Firecracker.GuestCID,
		GuestPort:                     cfg.Backends.Firecracker.GuestPort,
		LaunchSeconds:                 cfg.Backends.Firecracker.LaunchSeconds,
		MinimumCacheOutputVolumeBytes: int64(cfg.Backends.Firecracker.MinimumCacheOutputVolumeBytes),
	}
	if backendName == "darwin-vz" {
		out.KernelImagePath = cfg.Backends.DarwinVZ.KernelImage
		out.RootFSPath = cfg.Backends.DarwinVZ.RootFS
		out.MinimumRootFSBytes = int64(cfg.Backends.DarwinVZ.MinimumRootFSBytes)
		if out.MinimumRootFSBytes > 0 {
			out.MinimumRootFSBytesSource = backend.RootFSMinimumSourceConfig
		}
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
		out.MinimumCacheOutputVolumeBytes = int64(cfg.Backends.DarwinVZ.MinimumCacheOutputVolumeBytes)
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
				MinimumRootFSBytes            *ByteSize `yaml:"minimum_rootfs_bytes"`
				MinimumCacheOutputVolumeBytes *ByteSize `yaml:"minimum_cache_output_volume_bytes"`
			} `yaml:"darwin-vz"`
		} `yaml:"backends"`
	}{}
	if err := yaml.Unmarshal(raw, &presenceCfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}

	darwinVZMinRootFSBytes := cfg.Backends.DarwinVZ.MinimumRootFSBytes
	darwinVZMinRootFSBytesSet := presenceCfg.Backends.DarwinVZ.MinimumRootFSBytes != nil
	darwinVZMinCacheOutputVolumeBytes := cfg.Backends.DarwinVZ.MinimumCacheOutputVolumeBytes
	darwinVZMinCacheOutputVolumeBytesSet := presenceCfg.Backends.DarwinVZ.MinimumCacheOutputVolumeBytes != nil
	if darwinVZConfigIsZero(cfg.Backends.DarwinVZ) {
		if darwinVZConfigHasValues(rawCfg.Backends.DarwinVZLegacy) {
			cfg.Backends.DarwinVZ = rawCfg.Backends.DarwinVZLegacy
		}
	}
	if darwinVZMinRootFSBytesSet {
		cfg.Backends.DarwinVZ.MinimumRootFSBytes = darwinVZMinRootFSBytes
	}
	if darwinVZMinCacheOutputVolumeBytesSet {
		cfg.Backends.DarwinVZ.MinimumCacheOutputVolumeBytes = darwinVZMinCacheOutputVolumeBytes
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
	cfg.Auth = normalizeAuthConfig(cfg.Auth)
	cfg.Cache.Peers = normalizeCachePeers(cfg.Cache.Peers)
	cfg.Gateway.Git.CacheHosts = trimStringSlice(cfg.Gateway.Git.CacheHosts)
	cfg.Gateway.OCI.Registries = trimStringMap(cfg.Gateway.OCI.Registries)
	cfg.Gateway.Credentials.GitHubApp = normalizeGatewayGitHubAppCredentials(cfg.Gateway.Credentials.GitHubApp)
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
	if err := validateCacheConfig(cfg.Cache); err != nil {
		return err
	}
	if err := validateAuthConfig(cfg.Auth); err != nil {
		return err
	}
	if err := validateGatewayConfig(cfg.Gateway); err != nil {
		return err
	}
	if err := validateDarwinVZRuntimeConfig(cfg.Backends.DarwinVZ); err != nil {
		return err
	}
	if err := validateObservabilityConfig(cfg.Observability); err != nil {
		return err
	}
	if err := validateSandboxLifecycleConfig(cfg.SandboxLifecycle); err != nil {
		return err
	}
	return nil
}

func validateSandboxLifecycleConfig(cfg SandboxLifecycleConfig) error {
	if err := validateDurationSeconds("sandbox_lifecycle.idle_suspend_after_seconds", cfg.IdleSuspendAfterSeconds); err != nil {
		return err
	}
	if err := validateDurationSeconds("sandbox_lifecycle.wake_timeout_seconds", cfg.WakeTimeoutSeconds); err != nil {
		return err
	}
	return nil
}

func validateDurationSeconds(name string, seconds int64) error {
	if seconds < 0 {
		return fmt.Errorf("%s must be greater than or equal to 0", name)
	}
	const maxDurationSeconds = int64(1<<63-1) / int64(time.Second)
	if seconds > maxDurationSeconds {
		return fmt.Errorf("%s is too large", name)
	}
	return nil
}

func normalizeAuthConfig(cfg AuthConfig) AuthConfig {
	cfg.PolicyFile = strings.TrimSpace(cfg.PolicyFile)
	for i := range cfg.OIDC.Issuers {
		issuer := &cfg.OIDC.Issuers[i]
		issuer.Name = strings.TrimSpace(issuer.Name)
		issuer.Issuer = strings.TrimRight(strings.TrimSpace(issuer.Issuer), "/")
		issuer.JWKSURL = strings.TrimSpace(issuer.JWKSURL)
		issuer.Audiences = trimStringSlice(issuer.Audiences)
		issuer.AllowedAlgorithms = trimStringSlice(issuer.AllowedAlgorithms)
		if len(issuer.AllowedAlgorithms) == 0 && issuerConfigHasValues(*issuer) {
			issuer.AllowedAlgorithms = []string{"RS256"}
		}
		if issuer.ClockSkewSeconds == 0 && issuerConfigHasValues(*issuer) {
			issuer.ClockSkewSeconds = DefaultAuthOIDCClockSkewSeconds
		}
		if issuer.MaxTokenLifetimeSeconds == 0 && issuerConfigHasValues(*issuer) {
			issuer.MaxTokenLifetimeSeconds = DefaultAuthOIDCMaxTokenLifetimeSeconds
		}
	}
	return cfg
}

func issuerConfigHasValues(cfg AuthOIDCIssuerConfig) bool {
	return strings.TrimSpace(cfg.Name) != "" ||
		strings.TrimSpace(cfg.Issuer) != "" ||
		strings.TrimSpace(cfg.JWKSURL) != "" ||
		len(cfg.Audiences) > 0 ||
		len(cfg.AllowedAlgorithms) > 0 ||
		cfg.ClockSkewSeconds != 0 ||
		cfg.MaxTokenLifetimeSeconds != 0
}

func validateAuthConfig(cfg AuthConfig) error {
	configured := cfg.Required || strings.TrimSpace(cfg.PolicyFile) != "" || len(cfg.OIDC.Issuers) > 0
	if !configured {
		return nil
	}
	if len(cfg.OIDC.Issuers) == 0 {
		return errors.New("auth.oidc.issuers must contain at least one issuer when auth is configured")
	}
	if strings.TrimSpace(cfg.PolicyFile) == "" {
		return errors.New("auth.policy_file is required when auth is configured")
	}

	seenNames := map[string]struct{}{}
	seenIssuers := map[string]struct{}{}
	for i, issuer := range cfg.OIDC.Issuers {
		if strings.TrimSpace(issuer.Name) == "" {
			return fmt.Errorf("auth.oidc.issuers[%d].name is required", i)
		}
		if strings.ContainsAny(issuer.Name, " \t\r\n/") {
			return fmt.Errorf("auth.oidc.issuers[%d].name must not contain whitespace or slash", i)
		}
		if _, ok := seenNames[issuer.Name]; ok {
			return fmt.Errorf("duplicate auth.oidc.issuers[%d].name %q", i, issuer.Name)
		}
		seenNames[issuer.Name] = struct{}{}

		if strings.TrimSpace(issuer.Issuer) == "" {
			return fmt.Errorf("auth.oidc.issuers[%d].issuer is required", i)
		}
		if err := validateAuthURL(issuer.Issuer); err != nil {
			return fmt.Errorf("invalid auth.oidc.issuers[%d].issuer: %w", i, err)
		}
		if _, ok := seenIssuers[issuer.Issuer]; ok {
			return fmt.Errorf("duplicate auth.oidc.issuers[%d].issuer %q", i, issuer.Issuer)
		}
		seenIssuers[issuer.Issuer] = struct{}{}

		if len(issuer.Audiences) == 0 {
			return fmt.Errorf("auth.oidc.issuers[%d].audiences must contain at least one audience", i)
		}
		if strings.TrimSpace(issuer.JWKSURL) == "" {
			return fmt.Errorf("auth.oidc.issuers[%d].jwks_url is required", i)
		}
		if err := validateAuthURL(issuer.JWKSURL); err != nil {
			return fmt.Errorf("invalid auth.oidc.issuers[%d].jwks_url: %w", i, err)
		}
		for j, alg := range issuer.AllowedAlgorithms {
			switch alg {
			case "RS256":
			default:
				return fmt.Errorf("unsupported auth.oidc.issuers[%d].allowed_algorithms[%d] %q (expected RS256)", i, j, alg)
			}
		}
		if issuer.ClockSkewSeconds < 0 {
			return fmt.Errorf("auth.oidc.issuers[%d].clock_skew_seconds must be non-negative", i)
		}
		if issuer.MaxTokenLifetimeSeconds <= 0 {
			return fmt.Errorf("auth.oidc.issuers[%d].max_token_lifetime_seconds must be positive", i)
		}
	}
	return nil
}

func validateAuthURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(parsed.Hostname()) {
			return errors.New("http scheme is only allowed for loopback hosts")
		}
	default:
		return fmt.Errorf("unsupported scheme %q (expected https, or http for loopback)", parsed.Scheme)
	}
	if parsed.Host == "" {
		return errors.New("must include a host")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateHTTPURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("unsupported scheme %q (expected http or https)", parsed.Scheme)
	}
	if parsed.Host == "" {
		return errors.New("must include a host")
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

func normalizeCachePeers(peers []CachePeerConfig) []CachePeerConfig {
	if len(peers) == 0 {
		return nil
	}
	out := make([]CachePeerConfig, 0, len(peers))
	for _, peer := range peers {
		normalized := CachePeerConfig{
			URL:      strings.TrimSpace(peer.URL),
			TokenEnv: strings.TrimSpace(peer.TokenEnv),
		}
		if normalized.URL == "" && normalized.TokenEnv == "" {
			continue
		}
		out = append(out, normalized)
	}
	return out
}

func validateCacheConfig(cfg CacheConfig) error {
	for i, peer := range cfg.Peers {
		if strings.TrimSpace(peer.URL) == "" {
			return fmt.Errorf("cache.peers[%d].url is required", i)
		}
		parsed, err := url.Parse(peer.URL)
		if err != nil {
			return fmt.Errorf("invalid cache.peers[%d].url: %w", i, err)
		}
		switch parsed.Scheme {
		case "http", "https":
		default:
			return fmt.Errorf("unsupported cache.peers[%d].url scheme %q (expected http or https)", i, parsed.Scheme)
		}
		if parsed.Host == "" {
			return fmt.Errorf("cache.peers[%d].url must include a host", i)
		}
		if strings.TrimSpace(peer.TokenEnv) == "" {
			return fmt.Errorf("cache.peers[%d].token_env is required", i)
		}
	}
	return nil
}

func normalizeGatewayGitHubAppCredentials(cfg GatewayGitHubAppCredentialsConfig) GatewayGitHubAppCredentialsConfig {
	cfg.AppID = ScalarString(strings.TrimSpace(string(cfg.AppID)))
	cfg.InstallationID = ScalarString(strings.TrimSpace(string(cfg.InstallationID)))
	cfg.PrivateKeyFile = strings.TrimSpace(cfg.PrivateKeyFile)
	cfg.RepoPrefixes = trimStringSlice(cfg.RepoPrefixes)
	return cfg
}

func GatewayGitHubAppCredentialsConfigured(cfg GatewayGitHubAppCredentialsConfig) bool {
	return strings.TrimSpace(string(cfg.AppID)) != "" ||
		strings.TrimSpace(string(cfg.InstallationID)) != "" ||
		strings.TrimSpace(cfg.PrivateKeyFile) != "" ||
		len(cfg.RepoPrefixes) > 0
}

func validateGatewayConfig(cfg GatewayConfig) error {
	if err := validateGatewayGitHubAppCredentials(cfg.Credentials.GitHubApp); err != nil {
		return err
	}
	return nil
}

func validateGatewayGitHubAppCredentials(cfg GatewayGitHubAppCredentialsConfig) error {
	if !GatewayGitHubAppCredentialsConfigured(cfg) {
		return nil
	}
	if strings.TrimSpace(string(cfg.AppID)) == "" {
		return errors.New("gateway.credentials.github_app.app_id is required when GitHub App credentials are configured")
	}
	if strings.TrimSpace(string(cfg.InstallationID)) == "" {
		return errors.New("gateway.credentials.github_app.installation_id is required when GitHub App credentials are configured")
	}
	if strings.TrimSpace(cfg.PrivateKeyFile) == "" {
		return errors.New("gateway.credentials.github_app.private_key_file is required when GitHub App credentials are configured")
	}
	if len(cfg.RepoPrefixes) == 0 {
		return errors.New("gateway.credentials.github_app.repo_prefixes must contain at least one owner/ or owner/repo prefix")
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
	return !darwinVZConfigIsZero(cfg) || cfg.MinimumRootFSBytes > 0 || cfg.MinimumCacheOutputVolumeBytes > 0
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
