//go:build darwin

package darwinvz

import (
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
)

func TestResolveDarwinVZNetworkDefaultsToVMNetShared(t *testing.T) {
	t.Parallel()

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

func TestResolveDarwinVZNetworkAcceptsVMNetSharedWithDefaultSubnet(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

	_, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{
		DarwinVZNetworkMode:   darwinVZNetworkModeVMNetShared,
		DarwinVZNetworkSubnet: "198.19.0.0/16",
	})
	if err == nil {
		t.Fatal("expected non-RFC1918 subnet to fail")
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
