package darwinvz

import (
	"runtime"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
)

func TestCapabilitiesDeclareGuestNetworkInterfaceWithoutAllowlistFilteringByDefault(t *testing.T) {
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
	if caps[backend.CapabilityDNSControlOrEquivalent] {
		t.Fatalf("expected %s=false", backend.CapabilityDNSControlOrEquivalent)
	}
}

func TestCapabilitiesDeclareAllowlistFilteringForFileHandleMode(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("filehandle allowlist capability is only supported on darwin")
	}

	caps := (&Adapter{ConfiguredNetworkMode: darwinVZNetworkModeFileHandle}).Capabilities()

	if !caps[backend.CapabilityNetworkDefaultDeny] {
		t.Fatalf("expected %s=true", backend.CapabilityNetworkDefaultDeny)
	}
	if !caps[backend.CapabilityNetworkAllowlistEgress] {
		t.Fatalf("expected %s=true", backend.CapabilityNetworkAllowlistEgress)
	}
	if !caps[backend.CapabilityNetworkGuestInterface] {
		t.Fatalf("expected %s=true", backend.CapabilityNetworkGuestInterface)
	}
	if !caps[backend.CapabilityDNSControlOrEquivalent] {
		t.Fatalf("expected %s=true", backend.CapabilityDNSControlOrEquivalent)
	}
}
