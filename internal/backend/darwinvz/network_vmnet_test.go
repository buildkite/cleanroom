//go:build darwin

package darwinvz

import (
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
)

func TestResolveDarwinVZNetworkDefaultsToVMNetShared(t *testing.T) {
	prevSupported := darwinVZVMNetSharedSupported
	prevResolveHelper := resolveDarwinVZNetworkHelperPath
	prevHasVMNetEntitlement := helperHasVMNetworkingEntitlementForNetworking
	darwinVZVMNetSharedSupported = func() bool { return true }
	resolveDarwinVZNetworkHelperPath = func() (string, error) { return "/tmp/helper", nil }
	helperHasVMNetworkingEntitlementForNetworking = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() {
		darwinVZVMNetSharedSupported = prevSupported
		resolveDarwinVZNetworkHelperPath = prevResolveHelper
		helperHasVMNetworkingEntitlementForNetworking = prevHasVMNetEntitlement
	})

	got, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{})
	if err != nil {
		t.Fatalf("resolveDarwinVZNetwork returned error: %v", err)
	}
	if got.Mode != darwinVZNetworkModeVMNetShared {
		t.Fatalf("unexpected network mode: got %q want %q", got.Mode, darwinVZNetworkModeVMNetShared)
	}
	if got.SubnetCIDR != "" {
		t.Fatalf("expected default vmnet-shared mode to omit subnet, got %q", got.SubnetCIDR)
	}
}

func TestResolveDarwinVZNetworkDefaultsToNATWhenVMNetSharedUnsupported(t *testing.T) {
	prevSupported := darwinVZVMNetSharedSupported
	darwinVZVMNetSharedSupported = func() bool { return false }
	t.Cleanup(func() {
		darwinVZVMNetSharedSupported = prevSupported
	})

	got, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{})
	if err != nil {
		t.Fatalf("resolveDarwinVZNetwork returned error: %v", err)
	}
	if got.Mode != darwinVZNetworkModeNAT {
		t.Fatalf("unexpected network mode: got %q want %q", got.Mode, darwinVZNetworkModeNAT)
	}
	if got.SubnetCIDR != "" {
		t.Fatalf("expected default nat mode to omit subnet, got %q", got.SubnetCIDR)
	}
}

func TestResolveDarwinVZNetworkDefaultsToNATWhenHelperLacksVMNetEntitlement(t *testing.T) {
	prevSupported := darwinVZVMNetSharedSupported
	prevResolveHelper := resolveDarwinVZNetworkHelperPath
	prevHasVMNetEntitlement := helperHasVMNetworkingEntitlementForNetworking
	darwinVZVMNetSharedSupported = func() bool { return true }
	resolveDarwinVZNetworkHelperPath = func() (string, error) { return "/tmp/helper", nil }
	helperHasVMNetworkingEntitlementForNetworking = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() {
		darwinVZVMNetSharedSupported = prevSupported
		resolveDarwinVZNetworkHelperPath = prevResolveHelper
		helperHasVMNetworkingEntitlementForNetworking = prevHasVMNetEntitlement
	})

	got, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{})
	if err != nil {
		t.Fatalf("resolveDarwinVZNetwork returned error: %v", err)
	}
	if got.Mode != darwinVZNetworkModeNAT {
		t.Fatalf("unexpected network mode: got %q want %q", got.Mode, darwinVZNetworkModeNAT)
	}
}

func TestResolveDarwinVZNetworkAcceptsVMNetSharedWithDefaultSubnet(t *testing.T) {
	prevSupported := darwinVZVMNetSharedSupported
	darwinVZVMNetSharedSupported = func() bool { return true }
	t.Cleanup(func() {
		darwinVZVMNetSharedSupported = prevSupported
	})

	got, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{
		DarwinVZNetworkMode: darwinVZNetworkModeVMNetShared,
	})
	if err != nil {
		t.Fatalf("resolveDarwinVZNetwork returned error: %v", err)
	}
	if got.Mode != darwinVZNetworkModeVMNetShared {
		t.Fatalf("unexpected network mode: got %q want %q", got.Mode, darwinVZNetworkModeVMNetShared)
	}
	if got.SubnetCIDR != "" {
		t.Fatalf("expected default vmnet-shared subnet to stay empty, got %q", got.SubnetCIDR)
	}
}

func TestResolveDarwinVZNetworkAcceptsRFC1918CustomSubnet(t *testing.T) {
	prevSupported := darwinVZVMNetSharedSupported
	darwinVZVMNetSharedSupported = func() bool { return true }
	t.Cleanup(func() {
		darwinVZVMNetSharedSupported = prevSupported
	})

	got, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{
		DarwinVZNetworkMode:   darwinVZNetworkModeVMNetShared,
		DarwinVZNetworkSubnet: "10.233.0.0/16",
	})
	if err != nil {
		t.Fatalf("resolveDarwinVZNetwork returned error: %v", err)
	}
	if got.SubnetCIDR != "10.233.0.0/16" {
		t.Fatalf("unexpected subnet: got %q want %q", got.SubnetCIDR, "10.233.0.0/16")
	}
}

func TestResolveDarwinVZNetworkRejectsNonRFC1918Subnet(t *testing.T) {
	prevSupported := darwinVZVMNetSharedSupported
	darwinVZVMNetSharedSupported = func() bool { return true }
	t.Cleanup(func() {
		darwinVZVMNetSharedSupported = prevSupported
	})

	_, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{
		DarwinVZNetworkMode:   darwinVZNetworkModeVMNetShared,
		DarwinVZNetworkSubnet: "198.19.0.0/16",
	})
	if err == nil {
		t.Fatal("expected non-RFC1918 subnet to fail")
	}
}

func TestResolveDarwinVZNetworkRejectsVMNetSharedWhenUnsupported(t *testing.T) {
	prevSupported := darwinVZVMNetSharedSupported
	darwinVZVMNetSharedSupported = func() bool { return false }
	t.Cleanup(func() {
		darwinVZVMNetSharedSupported = prevSupported
	})

	_, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{
		DarwinVZNetworkMode: darwinVZNetworkModeVMNetShared,
	})
	if err == nil {
		t.Fatal("expected vmnet-shared to fail when unsupported")
	}
	if got, want := err.Error(), `"vmnet-shared" requires macOS 26 or later`; got != want {
		t.Fatalf("unexpected error: got %q want %q", got, want)
	}
}

func TestResolveDarwinVZNetworkRejectsSubnetInNATMode(t *testing.T) {
	t.Parallel()

	_, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{
		DarwinVZNetworkMode:   darwinVZNetworkModeNAT,
		DarwinVZNetworkSubnet: "10.233.0.0/16",
	})
	if err == nil {
		t.Fatal("expected nat mode with subnet to fail")
	}
}

func TestResolveDarwinVZNetworkRejectsUnknownMode(t *testing.T) {
	t.Parallel()

	_, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{
		DarwinVZNetworkMode: "bridged",
	})
	if err == nil {
		t.Fatal("expected unknown network mode to fail")
	}
}
