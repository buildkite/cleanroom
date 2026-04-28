package firecracker

import (
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
)

func TestCapabilitiesDeclareNetworkSupport(t *testing.T) {
	caps := New().Capabilities()

	if !caps[backend.CapabilityNetworkDefaultDeny] {
		t.Fatalf("expected %s=true", backend.CapabilityNetworkDefaultDeny)
	}
	if !caps[backend.CapabilityNetworkAllowlistEgress] {
		t.Fatalf("expected %s=true", backend.CapabilityNetworkAllowlistEgress)
	}
	if caps[backend.CapabilityNetworkStageScopedEgress] {
		t.Fatalf("expected %s=false", backend.CapabilityNetworkStageScopedEgress)
	}
	if !caps[backend.CapabilityDNSControlOrEquivalent] {
		t.Fatalf("expected %s=true", backend.CapabilityDNSControlOrEquivalent)
	}
	if !caps[backend.CapabilityNetworkGuestInterface] {
		t.Fatalf("expected %s=true", backend.CapabilityNetworkGuestInterface)
	}
}
