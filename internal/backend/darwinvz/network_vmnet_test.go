//go:build darwin

package darwinvz

import (
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
)

func TestResolveDarwinVZNetworkDefaultsToFileHandle(t *testing.T) {
	t.Parallel()

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

func TestResolveDarwinVZNetworkRejectsRemovedLegacyModes(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"nat", "vmnet-shared"} {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			_, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{
				DarwinVZNetworkMode: mode,
			})
			if err == nil {
				t.Fatalf("expected mode %q to fail", mode)
			}
			if !strings.Contains(err.Error(), `only "filehandle" is supported`) {
				t.Fatalf("unexpected error for mode %q: %v", mode, err)
			}
		})
	}
}

func TestResolveDarwinVZNetworkRejectsRemovedLegacySettings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  backend.FirecrackerConfig
		want string
	}{
		{
			name: "external interface",
			cfg: backend.FirecrackerConfig{
				DarwinVZNetworkMode:              darwinVZNetworkModeFileHandle,
				DarwinVZNetworkExternalInterface: "en0",
			},
			want: "external_interface",
		},
		{
			name: "nat and dns toggles",
			cfg: backend.FirecrackerConfig{
				DarwinVZNetworkMode:            darwinVZNetworkModeFileHandle,
				DarwinVZNetworkDisableNAT44:    true,
				DarwinVZNetworkDisableNAT66:    true,
				DarwinVZNetworkDisableDNSProxy: true,
			},
			want: "disable_nat44, disable_nat66, disable_dns_proxy",
		},
		{
			name: "router advertisement toggle",
			cfg: backend.FirecrackerConfig{
				DarwinVZNetworkMode:                       darwinVZNetworkModeFileHandle,
				DarwinVZNetworkDisableRouterAdvertisement: true,
			},
			want: "disable_router_advertisement",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := resolveDarwinVZNetwork(tt.cfg)
			if err == nil {
				t.Fatal("expected legacy setting to fail")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestResolveDarwinVZNetworkRejectsNonRFC1918Subnet(t *testing.T) {
	t.Parallel()

	_, err := resolveDarwinVZNetwork(backend.FirecrackerConfig{
		DarwinVZNetworkMode:   darwinVZNetworkModeFileHandle,
		DarwinVZNetworkSubnet: "198.19.0.0/16",
	})
	if err == nil {
		t.Fatal("expected non-RFC1918 subnet to fail")
	}
}
