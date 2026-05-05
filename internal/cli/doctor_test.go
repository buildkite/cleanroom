package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	backendfirecracker "github.com/buildkite/cleanroom/internal/backend/firecracker"
	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type doctorTestAdapter struct{}

func (doctorTestAdapter) Name() string { return "doctor-test" }

func (doctorTestAdapter) ProvisionSandbox(context.Context, backend.ProvisionRequest) error {
	return nil
}

func (doctorTestAdapter) RunInSandbox(context.Context, backend.ExecutionRequest, backend.OutputStream) (*backend.ExecutionResult, error) {
	return &backend.ExecutionResult{Message: "ok"}, nil
}

func (doctorTestAdapter) TerminateSandbox(context.Context, string) error { return nil }

func (doctorTestAdapter) Doctor(context.Context, backend.DoctorRequest) (*backend.DoctorReport, error) {
	return &backend.DoctorReport{
		Backend: "doctor-test",
		Checks: []backend.DoctorCheck{
			{Name: "backend_doctor_check", Status: "pass", Message: "ok"},
		},
	}, nil
}

func (doctorTestAdapter) Capabilities() map[string]bool {
	return map[string]bool{
		backend.CapabilityNetworkDefaultDeny:     true,
		backend.CapabilityNetworkAllowlistEgress: false,
		backend.CapabilityNetworkGuestInterface:  false,
	}
}

type doctorSnapshotAdapter struct{ doctorTestAdapter }

func (doctorSnapshotAdapter) CreateSnapshot(context.Context, backend.SnapshotRequest) (*backend.SnapshotResult, error) {
	return &backend.SnapshotResult{StorageRef: "/tmp/snapshot.ext4"}, nil
}

func (doctorSnapshotAdapter) ProvisionSandboxFromSnapshot(context.Context, backend.ProvisionFromSnapshotRequest) error {
	return nil
}

func (doctorSnapshotAdapter) DeleteSnapshot(context.Context, backend.DeleteSnapshotRequest) error {
	return nil
}

type doctorCacheOutputAdapter struct{ doctorTestAdapter }

func (doctorCacheOutputAdapter) Capabilities() map[string]bool {
	return map[string]bool{
		backend.CapabilityExecStreaming:               true,
		backend.CapabilitySandboxSnapshot:             true,
		backend.CapabilitySandboxFileDownload:         true,
		backend.CapabilitySandboxFileUpload:           true,
		backend.CapabilitySandboxPathStat:             true,
		backend.CapabilitySandboxTreeWalk:             true,
		backend.CapabilitySandboxFileRead:             true,
		backend.CapabilitySandboxFileWrite:            true,
		backend.CapabilitySandboxPathRemove:           true,
		backend.CapabilitySandboxArchiveRead:          true,
		backend.CapabilitySandboxArchiveWrite:         true,
		backend.CapabilityNetworkDefaultDeny:          true,
		backend.CapabilityNetworkAllowlistEgress:      true,
		backend.CapabilityNetworkStageScopedEgress:    true,
		backend.CapabilityDNSControlOrEquivalent:      true,
		backend.CapabilityNetworkGuestInterface:       true,
		backend.CapabilitySandboxPortDial:             true,
		backend.CapabilitySandboxCacheOutputVolumes:   true,
		backend.CapabilitySandboxCacheOutputFastClone: false,
		backend.CapabilitySandboxOverlayWriteCapture:  true,
	}
}

type doctorFailingLoader struct{}

func (doctorFailingLoader) LoadAndCompile(string) (*policy.CompiledPolicy, string, error) {
	return nil, "", errors.New("policy unavailable")
}

func (doctorFailingLoader) LoadRepository(string) (policy.RepositoryConfig, string, error) {
	return policy.RepositoryConfig{}, "", errors.New("policy unavailable")
}

type doctorStaticLoader struct{}

func (doctorStaticLoader) LoadAndCompile(cwd string) (*policy.CompiledPolicy, string, error) {
	return &policy.CompiledPolicy{Hash: "policy_hash"}, filepath.Join(cwd, "cleanroom.yaml"), nil
}

func (doctorStaticLoader) LoadRepository(string) (policy.RepositoryConfig, string, error) {
	return policy.RepositoryConfig{}, "", nil
}

func TestDoctorCommandJSONIncludesCapabilities(t *testing.T) {
	t.Setenv("CLEANROOM_GITHUB_TOKEN", "ghp_testtoken")
	t.Setenv("CLEANROOM_GITLAB_TOKEN", "")

	tmpDir := t.TempDir()
	stdoutPath := filepath.Join(tmpDir, "doctor.json")
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatalf("create stdout file: %v", err)
	}

	cmd := DoctorCommand{
		Backend: "doctor-test",
		JSON:    true,
	}
	err = cmd.Run(&runtimeContext{
		CWD:        tmpDir,
		Stdout:     stdout,
		Loader:     doctorFailingLoader{},
		Config:     runtimeconfig.Config{},
		ConfigPath: filepath.Join(tmpDir, "config.yaml"),
		Backends: map[string]backend.Adapter{
			"doctor-test": doctorTestAdapter{},
		},
	})
	if closeErr := stdout.Close(); closeErr != nil {
		t.Fatalf("close stdout file: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("DoctorCommand.Run returned error: %v", err)
	}

	raw, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatalf("read doctor output: %v", err)
	}

	var payload struct {
		Backend      string                `json:"backend"`
		Capabilities map[string]bool       `json:"capabilities"`
		Checks       []backend.DoctorCheck `json:"checks"`
		Gateway      struct {
			DefaultListen   string   `json:"default_listen"`
			DefaultPort     int      `json:"default_port"`
			Routes          []string `json:"routes"`
			CredentialHosts []string `json:"credential_hosts"`
		} `json:"gateway"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal doctor JSON: %v", err)
	}

	if payload.Backend != "doctor-test" {
		t.Fatalf("unexpected backend: got %q", payload.Backend)
	}
	if payload.Capabilities == nil {
		t.Fatal("expected capabilities map in doctor JSON")
	}
	if !payload.Capabilities[backend.CapabilityNetworkDefaultDeny] {
		t.Fatalf("expected %s=true", backend.CapabilityNetworkDefaultDeny)
	}
	if payload.Capabilities[backend.CapabilityNetworkGuestInterface] {
		t.Fatalf("expected %s=false", backend.CapabilityNetworkGuestInterface)
	}
	if payload.Gateway.DefaultListen != ":8170" {
		t.Fatalf("unexpected gateway default listen: %q", payload.Gateway.DefaultListen)
	}
	if payload.Gateway.DefaultPort != 8170 {
		t.Fatalf("unexpected gateway default port: %d", payload.Gateway.DefaultPort)
	}
	if len(payload.Gateway.Routes) != len(gateway.Routes()) {
		t.Fatalf("expected %d gateway routes, got %d (%v)", len(gateway.Routes()), len(payload.Gateway.Routes), payload.Gateway.Routes)
	}
	foundGitHub := false
	for _, h := range payload.Gateway.CredentialHosts {
		if h == "github.com" {
			foundGitHub = true
		}
	}
	if !foundGitHub {
		t.Fatalf("expected github.com in credential hosts, got %v", payload.Gateway.CredentialHosts)
	}

	foundCapabilityCheck := false
	for _, check := range payload.Checks {
		if check.Name == "capability_network_guest_interface" {
			foundCapabilityCheck = true
			if check.Status != "warn" {
				t.Fatalf("expected guest interface capability status warn, got %q", check.Status)
			}
		}
	}
	if !foundCapabilityCheck {
		t.Fatal("expected capability_network_guest_interface check in doctor output")
	}
}

func TestApplyRuntimeCapabilityOverridesConfiguresFastCloneBySnapshotDriver(t *testing.T) {
	baseCaps := map[string]bool{
		backend.CapabilitySandboxSnapshot:           true,
		backend.CapabilitySandboxCacheOutputVolumes: true,
	}

	tests := []struct {
		name        string
		backendName string
		config      runtimeconfig.Config
		wantFast    bool
		wantSnap    bool
	}{
		{
			name:        "darwin-vz default apfs",
			backendName: "darwin-vz",
			config: runtimeconfig.Config{
				Backends: runtimeconfig.Backends{
					DarwinVZ: runtimeconfig.DarwinVZConfig{
						Snapshots: runtimeconfig.SnapshotConfig{Enabled: true},
					},
				},
			},
			wantFast: true,
			wantSnap: true,
		},
		{
			name:        "darwin-vz file",
			backendName: "darwin-vz",
			config: runtimeconfig.Config{
				Backends: runtimeconfig.Backends{
					DarwinVZ: runtimeconfig.DarwinVZConfig{
						Snapshots: runtimeconfig.SnapshotConfig{Enabled: true, Driver: "file"},
					},
				},
			},
			wantFast: false,
			wantSnap: true,
		},
		{
			name:        "firecracker zfs",
			backendName: "firecracker",
			config: runtimeconfig.Config{
				Backends: runtimeconfig.Backends{
					Firecracker: runtimeconfig.FirecrackerConfig{
						Snapshots: runtimeconfig.SnapshotConfig{Enabled: true, Driver: "zfs", ZFSDataset: "tank/cleanroom"},
					},
				},
			},
			wantFast: true,
			wantSnap: true,
		},
		{
			name:        "firecracker zfs without dataset",
			backendName: "firecracker",
			config: runtimeconfig.Config{
				Backends: runtimeconfig.Backends{
					Firecracker: runtimeconfig.FirecrackerConfig{
						Snapshots: runtimeconfig.SnapshotConfig{Enabled: true, Driver: "zfs"},
					},
				},
			},
			wantFast: false,
			wantSnap: true,
		},
		{
			name:        "firecracker file",
			backendName: "firecracker",
			config: runtimeconfig.Config{
				Backends: runtimeconfig.Backends{
					Firecracker: runtimeconfig.FirecrackerConfig{
						Snapshots: runtimeconfig.SnapshotConfig{Enabled: true, Driver: "file"},
					},
				},
			},
			wantFast: false,
			wantSnap: true,
		},
		{
			name:        "snapshots disabled",
			backendName: "darwin-vz",
			config: runtimeconfig.Config{
				Backends: runtimeconfig.Backends{
					DarwinVZ: runtimeconfig.DarwinVZConfig{
						Snapshots: runtimeconfig.SnapshotConfig{Enabled: false, Driver: "apfs"},
					},
				},
			},
			wantFast: false,
			wantSnap: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyRuntimeCapabilityOverrides(baseCaps, tt.backendName, tt.config)
			if got[backend.CapabilitySandboxCacheOutputFastClone] != tt.wantFast {
				t.Fatalf("unexpected %s: got %t want %t", backend.CapabilitySandboxCacheOutputFastClone, got[backend.CapabilitySandboxCacheOutputFastClone], tt.wantFast)
			}
			if got[backend.CapabilitySandboxSnapshot] != tt.wantSnap {
				t.Fatalf("unexpected %s: got %t want %t", backend.CapabilitySandboxSnapshot, got[backend.CapabilitySandboxSnapshot], tt.wantSnap)
			}
		})
	}
}

func TestDoctorCommandReportsConfiguredFastCloneCapability(t *testing.T) {
	tmpDir := t.TempDir()
	stdoutPath := filepath.Join(tmpDir, "doctor.json")
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatalf("create stdout file: %v", err)
	}

	cmd := DoctorCommand{
		Backend: "darwin-vz",
		JSON:    true,
	}
	err = cmd.Run(&runtimeContext{
		CWD:    tmpDir,
		Stdout: stdout,
		Loader: doctorStaticLoader{},
		Config: runtimeconfig.Config{
			Backends: runtimeconfig.Backends{
				DarwinVZ: runtimeconfig.DarwinVZConfig{
					Snapshots: runtimeconfig.SnapshotConfig{Enabled: true},
				},
			},
		},
		ConfigPath: filepath.Join(tmpDir, "config.yaml"),
		Backends: map[string]backend.Adapter{
			"darwin-vz": doctorCacheOutputAdapter{},
		},
	})
	if closeErr := stdout.Close(); closeErr != nil {
		t.Fatalf("close stdout file: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("DoctorCommand.Run returned error: %v", err)
	}

	raw, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatalf("read doctor output: %v", err)
	}

	var payload struct {
		Capabilities map[string]bool       `json:"capabilities"`
		Checks       []backend.DoctorCheck `json:"checks"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal doctor JSON: %v", err)
	}

	if !payload.Capabilities[backend.CapabilitySandboxCacheOutputFastClone] {
		t.Fatalf("expected %s=true", backend.CapabilitySandboxCacheOutputFastClone)
	}

	foundFastCloneCheck := false
	for _, check := range payload.Checks {
		if check.Name != "capability_sandbox_cache_output_fast_clone" {
			continue
		}
		foundFastCloneCheck = true
		if check.Status != "pass" {
			t.Fatalf("expected fast clone capability check to pass, got %q", check.Status)
		}
		if !strings.Contains(check.Message, "supported by configured snapshot driver \"apfs\"") {
			t.Fatalf("expected apfs support message, got %q", check.Message)
		}
	}
	if !foundFastCloneCheck {
		t.Fatal("expected capability_sandbox_cache_output_fast_clone check in doctor output")
	}
}

func TestDoctorCommandTextUsesPolishedPlainOutput(t *testing.T) {
	tmpDir := t.TempDir()
	stdout, readStdout := makeStdoutCapture(t)

	cmd := DoctorCommand{
		Backend: "doctor-test",
	}
	err := cmd.Run(&runtimeContext{
		CWD:        tmpDir,
		Stdout:     stdout,
		Loader:     doctorFailingLoader{},
		Config:     runtimeconfig.Config{},
		ConfigPath: filepath.Join(tmpDir, "config.yaml"),
		Backends: map[string]backend.Adapter{
			"doctor-test": doctorTestAdapter{},
		},
	})
	if err != nil {
		t.Fatalf("DoctorCommand.Run returned error: %v", err)
	}

	out := readStdout()
	if !strings.Contains(out, "doctor report (doctor-test)") {
		t.Fatalf("expected doctor report title, got: %q", out)
	}
	if !strings.Contains(out, "✓ [pass] runtime_config:") {
		t.Fatalf("expected pass check line, got: %q", out)
	}
	if !strings.Contains(out, "! [warn] repository_policy:") {
		t.Fatalf("expected warn check line, got: %q", out)
	}
	if !strings.Contains(out, "✓ [pass] gateway_listen:") {
		t.Fatalf("expected gateway listen check line, got: %q", out)
	}
	if !strings.Contains(out, "✓ [pass] gateway_routes:") {
		t.Fatalf("expected gateway routes check line, got: %q", out)
	}
	if !strings.Contains(out, "summary: ") {
		t.Fatalf("expected summary line, got: %q", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("expected plain output without ANSI escapes, got: %q", out)
	}
}

func TestDoctorCommandHonorsRuntimeSnapshotCapabilityConfig(t *testing.T) {
	tmpDir := t.TempDir()
	stdoutPath := filepath.Join(tmpDir, "doctor.json")
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatalf("create stdout file: %v", err)
	}

	cmd := DoctorCommand{
		Backend: "firecracker",
		JSON:    true,
	}
	err = cmd.Run(&runtimeContext{
		CWD:    tmpDir,
		Stdout: stdout,
		Loader: doctorFailingLoader{},
		Config: runtimeconfig.Config{
			Backends: runtimeconfig.Backends{
				Firecracker: runtimeconfig.FirecrackerConfig{
					Snapshots: runtimeconfig.SnapshotConfig{Enabled: false},
				},
			},
		},
		ConfigPath: filepath.Join(tmpDir, "config.yaml"),
		Backends: map[string]backend.Adapter{
			"firecracker": doctorSnapshotAdapter{},
		},
	})
	if closeErr := stdout.Close(); closeErr != nil {
		t.Fatalf("close stdout file: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("DoctorCommand.Run returned error: %v", err)
	}

	raw, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatalf("read doctor output: %v", err)
	}

	var payload struct {
		Capabilities map[string]bool       `json:"capabilities"`
		Checks       []backend.DoctorCheck `json:"checks"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal doctor JSON: %v", err)
	}

	for _, key := range []string{
		backend.CapabilitySandboxSnapshot,
	} {
		if payload.Capabilities[key] {
			t.Fatalf("expected %s=false when runtime config disables snapshots", key)
		}
	}
	if _, ok := payload.Capabilities["sandbox.restore"]; ok {
		t.Fatalf("did not expect sandbox.restore capability key in doctor payload")
	}
	if _, ok := payload.Capabilities["sandbox.fork"]; ok {
		t.Fatalf("did not expect sandbox.fork capability key in doctor payload")
	}

	foundSnapshotCheck := false
	for _, check := range payload.Checks {
		if check.Name != "capability_sandbox_snapshot" {
			continue
		}
		foundSnapshotCheck = true
		if check.Status != "warn" {
			t.Fatalf("expected disabled snapshot capability check to warn, got %q", check.Status)
		}
		if !strings.Contains(check.Message, "disabled by runtime config") {
			t.Fatalf("expected disabled-by-config snapshot message, got %q", check.Message)
		}
		if !strings.Contains(check.Message, "backends.firecracker.snapshots.enabled: true") {
			t.Fatalf("expected snapshot enable hint in message, got %q", check.Message)
		}
	}
	if !foundSnapshotCheck {
		t.Fatal("expected capability_sandbox_snapshot check in doctor output")
	}
}

func TestDoctorCommandJSONIncludesEffectiveSnapshotConfig(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpDir, "state"))

	stdoutPath := filepath.Join(tmpDir, "doctor.json")
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatalf("create stdout file: %v", err)
	}

	cmd := DoctorCommand{
		Backend: "darwin-vz",
		JSON:    true,
	}
	err = cmd.Run(&runtimeContext{
		CWD:    tmpDir,
		Stdout: stdout,
		Loader: doctorFailingLoader{},
		Config: runtimeconfig.Config{
			Backends: runtimeconfig.Backends{
				DarwinVZ: runtimeconfig.DarwinVZConfig{
					Snapshots: runtimeconfig.SnapshotConfig{Enabled: true},
				},
			},
		},
		ConfigPath: filepath.Join(tmpDir, "config.yaml"),
		Backends: map[string]backend.Adapter{
			"darwin-vz": doctorSnapshotAdapter{},
		},
	})
	if closeErr := stdout.Close(); closeErr != nil {
		t.Fatalf("close stdout file: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("DoctorCommand.Run returned error: %v", err)
	}

	raw, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatalf("read doctor output: %v", err)
	}

	var payload struct {
		Snapshot struct {
			Enabled   bool   `json:"enabled"`
			Driver    string `json:"driver"`
			BaseDir   string `json:"base_dir"`
			Defaulted bool   `json:"defaulted"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal doctor JSON: %v", err)
	}

	if !payload.Snapshot.Enabled {
		t.Fatal("expected snapshot config enabled=true")
	}
	if got, want := payload.Snapshot.Driver, "apfs"; got != want {
		t.Fatalf("unexpected effective snapshot driver: got %q want %q", got, want)
	}
	if !payload.Snapshot.Defaulted {
		t.Fatal("expected darwin-vz snapshot driver to be marked defaulted")
	}
	if got, want := payload.Snapshot.BaseDir, filepath.Join(tmpDir, "state", "cleanroom", "snapshots"); got != want {
		t.Fatalf("unexpected effective snapshot base dir: got %q want %q", got, want)
	}
}

func TestDoctorCommandTextIncludesEffectiveSnapshotConfig(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpDir, "state"))

	stdout, readStdout := makeStdoutCapture(t)

	cmd := DoctorCommand{
		Backend: "darwin-vz",
	}
	err := cmd.Run(&runtimeContext{
		CWD:    tmpDir,
		Stdout: stdout,
		Loader: doctorFailingLoader{},
		Config: runtimeconfig.Config{
			Backends: runtimeconfig.Backends{
				DarwinVZ: runtimeconfig.DarwinVZConfig{
					Snapshots: runtimeconfig.SnapshotConfig{Enabled: false},
				},
			},
		},
		ConfigPath: filepath.Join(tmpDir, "config.yaml"),
		Backends: map[string]backend.Adapter{
			"darwin-vz": doctorSnapshotAdapter{},
		},
	})
	if err != nil {
		t.Fatalf("DoctorCommand.Run returned error: %v", err)
	}

	out := readStdout()
	if !strings.Contains(out, "snapshot_config: enabled=false driver=apfs (defaulted)") {
		t.Fatalf("expected snapshot config line in doctor output, got: %q", out)
	}
	if !strings.Contains(out, filepath.Join(tmpDir, "state", "cleanroom", "snapshots")) {
		t.Fatalf("expected snapshot base dir in doctor output, got: %q", out)
	}
}

func TestDoctorCommandReportsSupportedFirecrackerTierInJSON(t *testing.T) {
	stubFirecrackerHostSupport(t, func(context.Context, backend.FirecrackerConfig) backendfirecracker.HostSupport {
		return backendfirecracker.HostSupport{
			RuntimeUsable:   true,
			SnapshotsUsable: true,
			ZFSUsable:       true,
			ZFSDatasetRoot:  "tank/cleanroom",
		}
	})

	tmpDir := t.TempDir()
	stdoutPath := filepath.Join(tmpDir, "doctor.json")
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatalf("create stdout file: %v", err)
	}

	cmd := DoctorCommand{Backend: "firecracker", JSON: true}
	err = cmd.Run(&runtimeContext{
		CWD:    tmpDir,
		Stdout: stdout,
		Loader: doctorFailingLoader{},
		Config: runtimeconfig.Config{
			Backends: runtimeconfig.Backends{
				Firecracker: runtimeconfig.FirecrackerConfig{
					Snapshots: runtimeconfig.SnapshotConfig{Enabled: true, Driver: "zfs", ZFSDataset: "tank/cleanroom"},
				},
			},
		},
		ConfigPath: filepath.Join(tmpDir, "config.yaml"),
		Backends: map[string]backend.Adapter{
			"firecracker": doctorSnapshotAdapter{},
		},
	})
	if closeErr := stdout.Close(); closeErr != nil {
		t.Fatalf("close stdout file: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("DoctorCommand.Run returned error: %v", err)
	}

	raw, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatalf("read doctor output: %v", err)
	}

	var payload struct {
		Support struct {
			Tier              string `json:"tier"`
			Message           string `json:"message"`
			HostRuntimeUsable bool   `json:"host_runtime_usable"`
			SnapshotsUsable   bool   `json:"snapshots_usable"`
			ZFSUsable         bool   `json:"zfs_usable"`
			ZFSDatasetRoot    string `json:"zfs_dataset_root"`
		} `json:"support"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal doctor JSON: %v", err)
	}
	if got, want := payload.Support.Tier, "supported"; got != want {
		t.Fatalf("unexpected support tier: got %q want %q", got, want)
	}
	if !payload.Support.HostRuntimeUsable || !payload.Support.SnapshotsUsable || !payload.Support.ZFSUsable {
		t.Fatalf("expected support booleans to be true, got %+v", payload.Support)
	}
	if got, want := payload.Support.ZFSDatasetRoot, "tank/cleanroom"; got != want {
		t.Fatalf("unexpected support dataset root: got %q want %q", got, want)
	}
	if !strings.Contains(payload.Support.Message, "supported: zfs-backed firecracker layered caching") {
		t.Fatalf("unexpected support summary message: %q", payload.Support.Message)
	}
}

func TestDoctorCommandReportsDegradedFirecrackerTierInText(t *testing.T) {
	stubFirecrackerHostSupport(t, func(context.Context, backend.FirecrackerConfig) backendfirecracker.HostSupport {
		return backendfirecracker.HostSupport{
			RuntimeUsable:   true,
			SnapshotsUsable: true,
			ZFSUsable:       true,
			ZFSDatasetRoot:  "tank/cleanroom",
		}
	})

	tmpDir := t.TempDir()
	stdout, readStdout := makeStdoutCapture(t)

	cmd := DoctorCommand{Backend: "firecracker"}
	err := cmd.Run(&runtimeContext{
		CWD:    tmpDir,
		Stdout: stdout,
		Loader: doctorFailingLoader{},
		Config: runtimeconfig.Config{
			Backends: runtimeconfig.Backends{
				Firecracker: runtimeconfig.FirecrackerConfig{
					Snapshots: runtimeconfig.SnapshotConfig{Enabled: true, Driver: "file"},
				},
			},
		},
		ConfigPath: filepath.Join(tmpDir, "config.yaml"),
		Backends: map[string]backend.Adapter{
			"firecracker": doctorSnapshotAdapter{},
		},
	})
	if err != nil {
		t.Fatalf("DoctorCommand.Run returned error: %v", err)
	}

	out := readStdout()
	if !strings.Contains(out, "support_tier: degraded: firecracker layered caching is file-backed; warm restores still copy bytes") {
		t.Fatalf("expected degraded support tier line, got %q", out)
	}
	if !strings.Contains(out, "machine bootstrap can support zfs via tank/cleanroom") {
		t.Fatalf("expected degraded support tier hint, got %q", out)
	}
}

func TestDoctorCommandReportsUnsupportedFirecrackerTierWhenRuntimeMissing(t *testing.T) {
	stubFirecrackerHostSupport(t, func(context.Context, backend.FirecrackerConfig) backendfirecracker.HostSupport {
		return backendfirecracker.HostSupport{
			RuntimeUsable:   false,
			SnapshotsUsable: false,
			RuntimeMessage:  "missing required host commands: sudo",
			SnapshotMessage: "missing required host commands: sudo",
			ZFSMessage:      "missing required host commands: sudo",
		}
	})

	tmpDir := t.TempDir()
	stdout, readStdout := makeStdoutCapture(t)

	cmd := DoctorCommand{Backend: "firecracker"}
	err := cmd.Run(&runtimeContext{
		CWD:        tmpDir,
		Stdout:     stdout,
		Loader:     doctorFailingLoader{},
		Config:     runtimeconfig.Config{},
		ConfigPath: filepath.Join(tmpDir, "config.yaml"),
		Backends: map[string]backend.Adapter{
			"firecracker": doctorSnapshotAdapter{},
		},
	})
	if err != nil {
		t.Fatalf("DoctorCommand.Run returned error: %v", err)
	}

	out := readStdout()
	if !strings.Contains(out, "✗ [fail] support_tier: unsupported: machine bootstrap incomplete for firecracker host runtime: missing required host commands: sudo") {
		t.Fatalf("expected unsupported support tier line, got %q", out)
	}
}
