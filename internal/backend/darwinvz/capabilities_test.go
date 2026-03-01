package darwinvz

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
)

func TestCapabilitiesDeclareGuestNetworkInterfaceWithoutAllowlistFilteringByDefault(t *testing.T) {
	t.Setenv(networkFilterStatusPathEnv, filepath.Join(t.TempDir(), "missing-status.json"))

	caps := New().Capabilities()

	if !caps[backend.CapabilityNetworkDefaultDeny] {
		t.Fatalf("expected %s=true", backend.CapabilityNetworkDefaultDeny)
	}
	if caps[backend.CapabilityNetworkAllowlistEgress] {
		t.Fatalf("expected %s=false", backend.CapabilityNetworkAllowlistEgress)
	}
	if !caps[backend.CapabilityNetworkGuestInterface] {
		t.Fatalf("expected %s=true", backend.CapabilityNetworkGuestInterface)
	}
}

func TestCapabilitiesDeclareAllowlistFilteringWhenHostFilterEnabled(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "network-filter-status.json")
	t.Setenv(networkFilterStatusPathEnv, statusPath)

	status := []byte(`{"version":1,"available":true,"loaded":true,"enabled":true}`)
	if err := os.WriteFile(statusPath, status, 0o644); err != nil {
		t.Fatalf("write status file: %v", err)
	}

	caps := New().Capabilities()
	if !caps[backend.CapabilityNetworkAllowlistEgress] {
		t.Fatalf("expected %s=true", backend.CapabilityNetworkAllowlistEgress)
	}
}
