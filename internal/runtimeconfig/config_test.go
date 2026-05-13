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
      mode: filehandle
      subnet: 10.233.0.0/24
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
	if got, want := cfg.Backends.DarwinVZ.Network.Mode, "filehandle"; got != want {
		t.Fatalf("unexpected darwin-vz network mode: got %q want %q", got, want)
	}
	if got, want := cfg.Backends.DarwinVZ.Network.Subnet, "10.233.0.0/24"; got != want {
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

func TestLoadParsesCachePeers(t *testing.T) {
	cfg, err := loadConfigFromContent(t, `default_backend: firecracker
cache:
  peers:
    - url: " https://cleanroom-a.internal:8989 "
      token_env: " CLEANROOM_CACHE_PEER_TOKEN "
    - url: ""
      token_env: ""
    - url: http://127.0.0.1:8989
      token_env: CLEANROOM_LOCAL_TOKEN
`)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := len(cfg.Cache.Peers), 2; got != want {
		t.Fatalf("unexpected cache peer count: got %d want %d", got, want)
	}
	if got, want := cfg.Cache.Peers[0].URL, "https://cleanroom-a.internal:8989"; got != want {
		t.Fatalf("unexpected first peer URL: got %q want %q", got, want)
	}
	if got, want := cfg.Cache.Peers[0].TokenEnv, "CLEANROOM_CACHE_PEER_TOKEN"; got != want {
		t.Fatalf("unexpected first peer token env: got %q want %q", got, want)
	}
	if got, want := cfg.Cache.Peers[1].URL, "http://127.0.0.1:8989"; got != want {
		t.Fatalf("unexpected second peer URL: got %q want %q", got, want)
	}
	if got, want := cfg.Cache.Peers[1].TokenEnv, "CLEANROOM_LOCAL_TOKEN"; got != want {
		t.Fatalf("unexpected second peer token env: got %q want %q", got, want)
	}
}

func TestLoadRejectsInvalidCachePeers(t *testing.T) {
	tests := []struct {
		name    string
		peer    string
		wantErr string
	}{
		{
			name: "missing url",
			peer: `    - token_env: CLEANROOM_CACHE_PEER_TOKEN
`,
			wantErr: "cache.peers[0].url is required",
		},
		{
			name: "unsupported scheme",
			peer: `    - url: ftp://cleanroom-a.internal:8989
      token_env: CLEANROOM_CACHE_PEER_TOKEN
`,
			wantErr: `unsupported cache.peers[0].url scheme "ftp"`,
		},
		{
			name: "missing host",
			peer: `    - url: https:///cache-peer
      token_env: CLEANROOM_CACHE_PEER_TOKEN
`,
			wantErr: "cache.peers[0].url must include a host",
		},
		{
			name: "missing token env",
			peer: `    - url: https://cleanroom-a.internal:8989
`,
			wantErr: "cache.peers[0].token_env is required",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadConfigFromContent(t, `default_backend: firecracker
cache:
  peers:
`+tt.peer)
			if err == nil {
				t.Fatal("expected Load to reject invalid cache peer")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error to contain %q, got %v", tt.wantErr, err)
			}
		})
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

func TestLoadSupportsDarwinVZFileHandleNetworkMode(t *testing.T) {
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
      mode: filehandle
      subnet: 10.233.0.0/24
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := cfg.Backends.DarwinVZ.Network.Mode, "filehandle"; got != want {
		t.Fatalf("unexpected darwin-vz network mode: got %q want %q", got, want)
	}
	if got, want := cfg.Backends.DarwinVZ.Network.Subnet, "10.233.0.0/24"; got != want {
		t.Fatalf("unexpected darwin-vz network subnet: got %q want %q", got, want)
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

func TestLoadAllowsExplicitZeroDarwinVZMinimumRootFSBytesOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: darwin-vz
backends:
  darwin-vz:
    minimum_rootfs_bytes: 0
  darwin_vz:
    kernel_image: /tmp/legacy-kernel
    rootfs: /tmp/legacy-rootfs
    minimum_rootfs_bytes: 2GiB
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := int64(cfg.Backends.DarwinVZ.MinimumRootFSBytes), int64(0); got != want {
		t.Fatalf("unexpected darwin-vz minimum rootfs bytes: got %d want %d", got, want)
	}
}

func TestLoadSupportsDarwinVZMinimumCacheOutputVolumeBytes(t *testing.T) {
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
    minimum_cache_output_volume_bytes: 17179869184
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := int64(cfg.Backends.DarwinVZ.MinimumCacheOutputVolumeBytes), int64(17179869184); got != want {
		t.Fatalf("unexpected darwin-vz minimum cache output volume bytes: got %d want %d", got, want)
	}
}

func TestLoadSupportsDarwinVZMinimumCacheOutputVolumeBytesHumanString(t *testing.T) {
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
    minimum_cache_output_volume_bytes: 16GiB
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := int64(cfg.Backends.DarwinVZ.MinimumCacheOutputVolumeBytes), int64(16<<30); got != want {
		t.Fatalf("unexpected darwin-vz minimum cache output volume bytes: got %d want %d", got, want)
	}
}

func TestLoadRejectsInvalidDarwinVZMinimumCacheOutputVolumeBytes(t *testing.T) {
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
    minimum_cache_output_volume_bytes: giant
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected Load to reject invalid minimum_cache_output_volume_bytes")
	}
	if !strings.Contains(err.Error(), "invalid byte size") {
		t.Fatalf("expected error to mention invalid byte size, got %v", err)
	}
}

func TestLoadAllowsExplicitZeroDarwinVZMinimumCacheOutputVolumeBytesOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: darwin-vz
backends:
  darwin-vz:
    minimum_cache_output_volume_bytes: 0
  darwin_vz:
    kernel_image: /tmp/legacy-kernel
    rootfs: /tmp/legacy-rootfs
    minimum_cache_output_volume_bytes: 16GiB
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := int64(cfg.Backends.DarwinVZ.MinimumCacheOutputVolumeBytes), int64(0); got != want {
		t.Fatalf("unexpected darwin-vz minimum cache output volume bytes: got %d want %d", got, want)
	}
}

func TestLoadSupportsFirecrackerMinimumCacheOutputVolumeBytes(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: firecracker
backends:
  firecracker:
    minimum_cache_output_volume_bytes: 17179869184
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := int64(cfg.Backends.Firecracker.MinimumCacheOutputVolumeBytes), int64(17179869184); got != want {
		t.Fatalf("unexpected firecracker minimum cache output volume bytes: got %d want %d", got, want)
	}
}

func TestLoadTrimsObservabilityConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `observability:
  enabled: true
  deployment_environment: " ci "
  logs:
    format: " JSON "
  otlp:
    endpoint: " https://otel.example.test:4318 "
    protocol: " http/protobuf "
    headers:
      " x-otlp-token ": " secret "
  traces:
    sampling:
      mode: " parentbased_traceidratio "
      ratio: 0.5
    url_template: " https://jaeger.example.test/trace/{{.TraceID}} "
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cfg.Observability.Enabled {
		t.Fatal("expected observability to be enabled")
	}
	if got, want := cfg.Observability.DeploymentEnvironment, "ci"; got != want {
		t.Fatalf("unexpected deployment environment: got %q want %q", got, want)
	}
	if got, want := cfg.Observability.Logs.Format, "json"; got != want {
		t.Fatalf("unexpected log format: got %q want %q", got, want)
	}
	if got, want := cfg.Observability.OTLP.Endpoint, "https://otel.example.test:4318"; got != want {
		t.Fatalf("unexpected otlp endpoint: got %q want %q", got, want)
	}
	if got, want := cfg.Observability.OTLP.Protocol, "http/protobuf"; got != want {
		t.Fatalf("unexpected otlp protocol: got %q want %q", got, want)
	}
	if got, want := cfg.Observability.OTLP.Headers["x-otlp-token"], "secret"; got != want {
		t.Fatalf("unexpected otlp header: got %q want %q", got, want)
	}
	if got, want := cfg.Observability.Traces.Sampling.Mode, "parentbased_traceidratio"; got != want {
		t.Fatalf("unexpected sampling mode: got %q want %q", got, want)
	}
	if cfg.Observability.Traces.Sampling.Ratio == nil {
		t.Fatal("expected sampling ratio to be set")
	}
	if got, want := *cfg.Observability.Traces.Sampling.Ratio, 0.5; got != want {
		t.Fatalf("unexpected sampling ratio: got %v want %v", got, want)
	}
	if got, want := cfg.Observability.Traces.URLTemplate, "https://jaeger.example.test/trace/{{.TraceID}}"; got != want {
		t.Fatalf("unexpected trace url template: got %q want %q", got, want)
	}
}

func TestLoadSupportsOTLPObservabilityConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `observability:
  enabled: true
  otlp:
    endpoint: " http://localhost:4318 "
    protocol: " http/protobuf "
    headers:
      " x-otlp-token ": " secret "
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cfg.Observability.Enabled {
		t.Fatal("expected observability to be enabled")
	}
	if got, want := cfg.Observability.OTLP.Endpoint, "http://localhost:4318"; got != want {
		t.Fatalf("unexpected otlp endpoint: got %q want %q", got, want)
	}
	if got, want := cfg.Observability.OTLP.Protocol, "http/protobuf"; got != want {
		t.Fatalf("unexpected otlp protocol: got %q want %q", got, want)
	}
	if got, want := cfg.Observability.OTLP.Headers["x-otlp-token"], "secret"; got != want {
		t.Fatalf("unexpected otlp header: got %q want %q", got, want)
	}
}

func TestLoadDefaultsObservabilityLogFormatToText(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `observability:
  logs: {}
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := cfg.Observability.Logs.Format, "text"; got != want {
		t.Fatalf("unexpected log format: got %q want %q", got, want)
	}
}

func TestLoadRejectsUnsupportedObservabilityLogFormat(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `observability:
  logs:
    format: logfmt
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected Load to reject unsupported log format")
	}
	if !strings.Contains(err.Error(), "observability.logs.format") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadSupportsObservabilityTraceURLTemplate(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `observability:
  enabled: true
  otlp:
    endpoint: http://localhost:4318
  traces:
    url_template: " https://jaeger.example.test/trace/{{.TraceID}}?execution={{.ExecutionID}} "
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := cfg.Observability.Traces.URLTemplate, "https://jaeger.example.test/trace/{{.TraceID}}?execution={{.ExecutionID}}"; got != want {
		t.Fatalf("unexpected trace url template: got %q want %q", got, want)
	}
	traceURL, err := RenderTraceURL(cfg.Observability, "0123456789abcdef0123456789abcdef", "exec-123", "sandbox-123")
	if err != nil {
		t.Fatalf("RenderTraceURL returned error: %v", err)
	}
	if got, want := traceURL, "https://jaeger.example.test/trace/0123456789abcdef0123456789abcdef?execution=exec-123"; got != want {
		t.Fatalf("unexpected rendered trace url: got %q want %q", got, want)
	}
}

func TestLoadRejectsInvalidObservabilityTraceURLTemplateWhenEnabled(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `observability:
  enabled: true
  otlp:
    endpoint: http://localhost:4318
  traces:
    url_template: "https://jaeger.example.test/trace/{{.UnknownField}}"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected Load to reject invalid observability trace url template")
	}
	if !strings.Contains(err.Error(), "observability.traces.url_template") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadRejectsUnsupportedObservabilityOTLPProtocol(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `observability:
  enabled: true
  otlp:
    endpoint: http://localhost:4318
    protocol: banana
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected Load to reject unsupported observability OTLP protocol")
	}
	if !strings.Contains(err.Error(), "observability.otlp.protocol") {
		t.Fatalf("expected error to mention observability.otlp.protocol, got %v", err)
	}
}

func TestLoadRejectsObservabilityOTLPHTTPPathWhenEnabled(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `observability:
  enabled: true
  otlp:
    endpoint: https://collector.example.test/v1/traces
    protocol: http/protobuf
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected Load to reject observability.otlp.endpoint with a path for OTLP HTTP")
	}
	if !strings.Contains(err.Error(), "must not include a path") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadAllowsUnsupportedObservabilityConfigWhenDisabled(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `observability:
  enabled: false
  otlp:
    protocol: " banana "
  traces:
    exporter: " honeycomb "
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Observability.Enabled {
		t.Fatal("expected observability to remain disabled")
	}
	if got, want := cfg.Observability.OTLP.Protocol, "banana"; got != want {
		t.Fatalf("unexpected trimmed otlp protocol: got %q want %q", got, want)
	}
	if got, want := cfg.Observability.Traces.Exporter, "honeycomb"; got != want {
		t.Fatalf("unexpected trimmed trace exporter: got %q want %q", got, want)
	}
}

func TestLoadRejectsMissingOTLPEndpointWhenEnabled(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `observability:
  enabled: true
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected Load to reject missing observability.otlp.endpoint")
	}
	if !strings.Contains(err.Error(), "missing observability.otlp.endpoint") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadRejectsUnsupportedObservabilityTraceExporter(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `observability:
  enabled: true
  otlp:
    endpoint: http://localhost:4318
  traces:
    exporter: zipkin
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected Load to reject unsupported observability.traces.exporter")
	}
	if !strings.Contains(err.Error(), "unsupported observability.traces.exporter") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadRejectsUnsupportedObservabilitySamplingMode(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `observability:
  enabled: true
  otlp:
    endpoint: http://localhost:4318
  traces:
    sampling:
      mode: banana
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected Load to reject unsupported observability sampling mode")
	}
	if !strings.Contains(err.Error(), "observability.traces.sampling.mode") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadRejectsOutOfRangeObservabilitySamplingRatio(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `observability:
  enabled: true
  otlp:
    endpoint: http://localhost:4318
  traces:
    sampling:
      ratio: 2
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected Load to reject out-of-range observability sampling ratio")
	}
	if !strings.Contains(err.Error(), "observability.traces.sampling.ratio") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadTrimsGatewayGitCacheHosts(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `gateway:
  git:
    cache_hosts:
      - " github.com "
      - ""
      - " gitlab.com "
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := len(cfg.Gateway.Git.CacheHosts), 2; got != want {
		t.Fatalf("unexpected cache host count: got %d want %d", got, want)
	}
	if got, want := cfg.Gateway.Git.CacheHosts[0], "github.com"; got != want {
		t.Fatalf("unexpected first cache host: got %q want %q", got, want)
	}
	if got, want := cfg.Gateway.Git.CacheHosts[1], "gitlab.com"; got != want {
		t.Fatalf("unexpected second cache host: got %q want %q", got, want)
	}
}

func TestLoadTrimsGatewayOCIRegistries(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `gateway:
  oci:
    registries:
      " ghcr.io ": " https://ghcr.io/ "
      "": "https://example.invalid"
      " registry.internal:5000 ": " registry.internal:5000 "
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := len(cfg.Gateway.OCI.Registries), 2; got != want {
		t.Fatalf("unexpected registry mapping count: got %d want %d", got, want)
	}
	if got, want := cfg.Gateway.OCI.Registries["ghcr.io"], "https://ghcr.io/"; got != want {
		t.Fatalf("unexpected ghcr registry mapping: got %q want %q", got, want)
	}
	if got, want := cfg.Gateway.OCI.Registries["registry.internal:5000"], "registry.internal:5000"; got != want {
		t.Fatalf("unexpected internal registry mapping: got %q want %q", got, want)
	}
}

func TestLoadObservabilitySupportsZeroSamplingRatio(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `observability:
  traces:
    sampling:
      ratio: 0
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Observability.Traces.Sampling.Ratio == nil {
		t.Fatal("expected zero sampling ratio to be preserved")
	}
	if got, want := *cfg.Observability.Traces.Sampling.Ratio, 0.0; got != want {
		t.Fatalf("unexpected zero sampling ratio: got %v want %v", got, want)
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

func TestLoadSupportsLegacyDarwinVZMinimumCacheOutputVolumeBytesOnly(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: darwin-vz
backends:
  darwin_vz:
    minimum_cache_output_volume_bytes: 16GiB
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := int64(cfg.Backends.DarwinVZ.MinimumCacheOutputVolumeBytes), int64(16<<30); got != want {
		t.Fatalf("unexpected darwin-vz minimum cache output volume bytes: got %d want %d", got, want)
	}
}

func TestLoadRejectsInvalidLegacyDarwinVZMinimumRootFSBytes(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: darwin-vz
backends:
  darwin_vz:
    minimum_rootfs_bytes: 2GIBB
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected Load to reject invalid legacy minimum_rootfs_bytes")
	}
	if !strings.Contains(err.Error(), "invalid byte size") {
		t.Fatalf("expected error to mention invalid byte size, got %v", err)
	}
}

func TestLoadSupportsLargeExactDarwinVZMinimumRootFSBytes(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: darwin-vz
backends:
  darwin-vz:
    minimum_rootfs_bytes: 9007199254740993b
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := int64(cfg.Backends.DarwinVZ.MinimumRootFSBytes), int64(9007199254740993); got != want {
		t.Fatalf("unexpected darwin-vz minimum rootfs bytes: got %d want %d", got, want)
	}
}

func TestLoadSupportsMaxInt64DarwinVZMinimumRootFSBytes(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: darwin-vz
backends:
  darwin-vz:
    minimum_rootfs_bytes: 9223372036854775807b
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := int64(cfg.Backends.DarwinVZ.MinimumRootFSBytes), int64(9223372036854775807); got != want {
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

func TestLoadDefaultsBackendWhenMissingAndCannotBeInferred(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := "backends: {}\n"
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

func TestLoadUsesOnlyDefinedBackendWhenDefaultBackendMissing(t *testing.T) {
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
	if got, want := cfg.DefaultBackend, "firecracker"; got != want {
		t.Fatalf("unexpected default backend: got %q want %q", got, want)
	}
}

func TestLoadRejectsUnsupportedDefaultBackend(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: podman
backends:
  firecracker:
    binary_path: firecracker
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected Load to reject unsupported default backend")
	}
	if !strings.Contains(err.Error(), "unsupported default_backend") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadRejectsUnknownConfigFields(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: firecracker
backends:
  firecracker:
    memory_mb: 1024
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected unknown config field to be rejected")
	}
	if !strings.Contains(err.Error(), "memory_mb") {
		t.Fatalf("expected error to name unknown field, got %v", err)
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

func TestLoadRejectsInvalidControlHost(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `control_host: not-an-endpoint
backends:
  firecracker:
    binary_path: firecracker
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected Load to reject invalid control_host")
	}
	if !strings.Contains(err.Error(), "invalid control_host") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadRejectsUnsupportedDarwinVZNetworkMode(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	content := `default_backend: darwin-vz
backends:
  darwin-vz:
    network:
      mode: vmnet
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected Load to reject unsupported darwin-vz network mode")
	}
	if !strings.Contains(err.Error(), "unsupported darwin-vz network mode") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadPathLoadsExplicitPath(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "runtime.yaml")
	content := `default_backend: firecracker
backends:
  firecracker:
    binary_path: firecracker
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, resolvedPath, err := LoadPath(configPath)
	if err != nil {
		t.Fatalf("LoadPath returned error: %v", err)
	}
	if got, want := resolvedPath, configPath; got != want {
		t.Fatalf("unexpected resolved path: got %q want %q", got, want)
	}
	if got, want := cfg.DefaultBackend, "firecracker"; got != want {
		t.Fatalf("unexpected default backend: got %q want %q", got, want)
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
				Network: DarwinVZNetworkConfig{
					Mode:   "filehandle",
					Subnet: "10.233.0.0/24",
				},
				Services:      ServicesConfig{Docker: DockerServiceConfig{StartupTimeoutSeconds: 20, StorageDriver: "vzfs", IPTables: false}},
				Snapshots:     SnapshotConfig{Enabled: false, Driver: "apfs", BaseDir: "/darwin/snapshots", QuiesceTimeoutSeconds: 22},
				VCPUs:         4,
				MemoryMiB:     2048,
				GuestPort:     10701,
				LaunchSeconds: 45,
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
	if got, want := darwinCfg.DarwinVZNetworkMode, "filehandle"; got != want {
		t.Fatalf("unexpected darwin-vz network mode: got %q want %q", got, want)
	}
	if got, want := darwinCfg.DarwinVZNetworkSubnet, "10.233.0.0/24"; got != want {
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

func loadConfigFromContent(t *testing.T, content string) (Config, error) {
	t.Helper()

	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	configPath := filepath.Join(tmp, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, _, err := Load()
	return cfg, err
}
