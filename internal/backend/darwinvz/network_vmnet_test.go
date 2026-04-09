//go:build darwin

package darwinvz

import (
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
)

func TestResolveDarwinVZNetworkDefaultsToFileHandle(t *testing.T) {
	prevSupported := darwinVZVMNetSharedSupported
	darwinVZVMNetSharedSupported = func() bool { return true }
	t.Cleanup(func() {
		darwinVZVMNetSharedSupported = prevSupported
	})

	got, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{})
	if err != nil {
		t.Fatalf("resolveDarwinVZNetwork returned error: %v", err)
	}
	if got.Mode != darwinVZNetworkModeFileHandle {
		t.Fatalf("unexpected network mode: got %q want %q", got.Mode, darwinVZNetworkModeFileHandle)
	}
	if got.SubnetCIDR != darwinVZFileHandleDefaultSubnetCIDR {
		t.Fatalf("unexpected default file-handle subnet: got %q want %q", got.SubnetCIDR, darwinVZFileHandleDefaultSubnetCIDR)
	}
}

func TestResolveDarwinVZNetworkDefaultsToFileHandleWhenVMNetSharedUnsupported(t *testing.T) {
	prevSupported := darwinVZVMNetSharedSupported
	darwinVZVMNetSharedSupported = func() bool { return false }
	t.Cleanup(func() {
		darwinVZVMNetSharedSupported = prevSupported
	})

	got, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{})
	if err != nil {
		t.Fatalf("resolveDarwinVZNetwork returned error: %v", err)
	}
	if got.Mode != darwinVZNetworkModeFileHandle {
		t.Fatalf("unexpected network mode: got %q want %q", got.Mode, darwinVZNetworkModeFileHandle)
	}
	if got.SubnetCIDR != darwinVZFileHandleDefaultSubnetCIDR {
		t.Fatalf("unexpected default file-handle subnet: got %q want %q", got.SubnetCIDR, darwinVZFileHandleDefaultSubnetCIDR)
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

func TestResolveDarwinVZNetworkAcceptsFileHandleWithDefaultSubnet(t *testing.T) {
	t.Parallel()

	got, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{
		DarwinVZNetworkMode: darwinVZNetworkModeFileHandle,
	})
	if err != nil {
		t.Fatalf("resolveDarwinVZNetwork returned error: %v", err)
	}
	if got.Mode != darwinVZNetworkModeFileHandle {
		t.Fatalf("unexpected network mode: got %q want %q", got.Mode, darwinVZNetworkModeFileHandle)
	}
	if got.SubnetCIDR != darwinVZFileHandleDefaultSubnetCIDR {
		t.Fatalf("unexpected default file-handle subnet: got %q want %q", got.SubnetCIDR, darwinVZFileHandleDefaultSubnetCIDR)
	}
}

func TestResolveDarwinVZNetworkAcceptsFileHandleWithCustomSubnet(t *testing.T) {
	t.Parallel()

	got, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{
		DarwinVZNetworkMode:   darwinVZNetworkModeFileHandle,
		DarwinVZNetworkSubnet: "10.233.0.0/16",
	})
	if err != nil {
		t.Fatalf("resolveDarwinVZNetwork returned error: %v", err)
	}
	if got.SubnetCIDR != "10.233.0.0/16" {
		t.Fatalf("unexpected subnet: got %q want %q", got.SubnetCIDR, "10.233.0.0/16")
	}
}

func TestResolveDarwinVZNetworkAcceptsRFC1918CustomSubnet(t *testing.T) {
	prevSupported := darwinVZVMNetSharedSupported
	darwinVZVMNetSharedSupported = func() bool { return true }
	t.Cleanup(func() {
		darwinVZVMNetSharedSupported = prevSupported
	})

	got, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{
		DarwinVZNetworkMode:                       darwinVZNetworkModeVMNetShared,
		DarwinVZNetworkSubnet:                     "10.233.0.0/16",
		DarwinVZNetworkExternalInterface:          "en0",
		DarwinVZNetworkDisableNAT44:               true,
		DarwinVZNetworkDisableNAT66:               true,
		DarwinVZNetworkDisableDNSProxy:            true,
		DarwinVZNetworkDisableRouterAdvertisement: true,
	})
	if err != nil {
		t.Fatalf("resolveDarwinVZNetwork returned error: %v", err)
	}
	if got.SubnetCIDR != "10.233.0.0/16" {
		t.Fatalf("unexpected subnet: got %q want %q", got.SubnetCIDR, "10.233.0.0/16")
	}
	if got.ExternalInterface != "en0" {
		t.Fatalf("unexpected external interface: got %q want %q", got.ExternalInterface, "en0")
	}
	if !got.DisableNAT44 || !got.DisableNAT66 || !got.DisableDNSProxy || !got.DisableRouterAdvertisement {
		t.Fatalf("expected vmnet diagnostic flags to be enabled, got %#v", got)
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

func TestResolveDarwinVZNetworkRejectsVMNetOnlyFieldsInNATMode(t *testing.T) {
	t.Parallel()

	_, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{
		DarwinVZNetworkMode:              darwinVZNetworkModeNAT,
		DarwinVZNetworkExternalInterface: "en0",
	})
	if err == nil {
		t.Fatal("expected nat mode with external interface to fail")
	}

	_, err = resolveDarwinVZNetwork(backend.FirecrackerConfig{
		DarwinVZNetworkMode:            darwinVZNetworkModeNAT,
		DarwinVZNetworkDisableNAT44:    true,
		DarwinVZNetworkDisableNAT66:    true,
		DarwinVZNetworkDisableDNSProxy: true,
	})
	if err == nil {
		t.Fatal("expected nat mode with vmnet-only flags to fail")
	}
}

func TestResolveDarwinVZNetworkRejectsVMNetOnlyFieldsInFileHandleMode(t *testing.T) {
	t.Parallel()

	_, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{
		DarwinVZNetworkMode:              darwinVZNetworkModeFileHandle,
		DarwinVZNetworkExternalInterface: "en0",
	})
	if err == nil {
		t.Fatal("expected filehandle mode with external interface to fail")
	}

	_, err = resolveDarwinVZNetwork(backend.FirecrackerConfig{
		DarwinVZNetworkMode:            darwinVZNetworkModeFileHandle,
		DarwinVZNetworkDisableNAT44:    true,
		DarwinVZNetworkDisableNAT66:    true,
		DarwinVZNetworkDisableDNSProxy: true,
	})
	if err == nil {
		t.Fatal("expected filehandle mode with vmnet-only flags to fail")
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
