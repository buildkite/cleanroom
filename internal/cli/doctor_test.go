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
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type doctorTestAdapter struct{}

func (doctorTestAdapter) Name() string { return "doctor-test" }

func (doctorTestAdapter) Run(context.Context, backend.ExecutionRequest) (*backend.ExecutionResult, error) {
	return &backend.ExecutionResult{Message: "ok"}, nil
}

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

func (doctorSnapshotAdapter) ProvisionSandbox(context.Context, backend.ProvisionRequest) error {
	return nil
}

func (doctorSnapshotAdapter) RunInSandbox(context.Context, backend.ExecutionRequest, backend.OutputStream) (*backend.ExecutionResult, error) {
	return &backend.ExecutionResult{Message: "ok"}, nil
}

func (doctorSnapshotAdapter) TerminateSandbox(context.Context, string) error {
	return nil
}

func (doctorSnapshotAdapter) CreateSnapshot(context.Context, backend.SnapshotRequest) (*backend.SnapshotResult, error) {
	return &backend.SnapshotResult{StorageRef: "/tmp/snapshot.ext4"}, nil
}

func (doctorSnapshotAdapter) ProvisionSandboxFromSnapshot(context.Context, backend.ProvisionFromSnapshotRequest) error {
	return nil
}

func (doctorSnapshotAdapter) DeleteSnapshot(context.Context, backend.DeleteSnapshotRequest) error {
	return nil
}

type doctorFailingLoader struct{}

func (doctorFailingLoader) LoadAndCompile(string) (*policy.CompiledPolicy, string, error) {
	return nil, "", errors.New("policy unavailable")
}

func (doctorFailingLoader) LoadRepository(string) (policy.RepositoryConfig, string, error) {
	return policy.RepositoryConfig{}, "", errors.New("policy unavailable")
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
		Backend          string                `json:"backend"`
		CleanroomVersion string                `json:"cleanroom_version"`
		Capabilities     map[string]bool       `json:"capabilities"`
		Checks           []backend.DoctorCheck `json:"checks"`
		Gateway          struct {
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
	if payload.CleanroomVersion != "dev" {
		t.Fatalf("unexpected cleanroom version: got %q", payload.CleanroomVersion)
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
	if len(payload.Gateway.Routes) != 4 {
		t.Fatalf("expected 4 gateway routes, got %d (%v)", len(payload.Gateway.Routes), payload.Gateway.Routes)
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
	if !strings.Contains(out, "cleanroom_version: cleanroom version dev") {
		t.Fatalf("expected version check line, got: %q", out)
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
