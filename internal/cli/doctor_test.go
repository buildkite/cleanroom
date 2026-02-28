package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
