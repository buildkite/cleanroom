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

func (doctorTestAdapter) Run(context.Context, backend.RunRequest) (*backend.RunResult, error) {
	return &backend.RunResult{Message: "ok"}, nil
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

type doctorFailingLoader struct{}

func (doctorFailingLoader) LoadAndCompile(string) (*policy.CompiledPolicy, string, error) {
	return nil, "", errors.New("policy unavailable")
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
	if len(payload.Gateway.Routes) != 4 {
		t.Fatalf("expected 4 gateway routes, got %d (%v)", len(payload.Gateway.Routes), payload.Gateway.Routes)
	}
	if len(payload.Gateway.CredentialHosts) != 1 || payload.Gateway.CredentialHosts[0] != "github.com" {
		t.Fatalf("unexpected gateway credential hosts: %v", payload.Gateway.CredentialHosts)
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
