package darwinvz

import (
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
)

func TestCapabilitiesDeclareGuestNetworkInterfaceWithoutAllowlistFiltering(t *testing.T) {
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
