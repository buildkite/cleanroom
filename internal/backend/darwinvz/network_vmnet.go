//go:build darwin

package darwinvz

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
)

const darwinVZFileHandleDefaultSubnetCIDR = "10.233.0.0/24"

type darwinVZNetwork struct {
	Mode       string
	SubnetCIDR string
}

var darwinVZRFC1918Prefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

func resolveDarwinVZNetwork(cfg backend.FirecrackerConfig) (darwinVZNetwork, error) {
	if err := validateDarwinVZNetworkSelection(cfg); err != nil {
		return darwinVZNetwork{}, err
	}
	mode := darwinVZConfiguredOrDefaultNetworkMode(cfg.DarwinVZNetworkMode)
	subnet := strings.TrimSpace(cfg.DarwinVZNetworkSubnet)
	switch mode {
	case darwinVZNetworkModeFileHandle:
		normalizedSubnet, err := normalizeDarwinVZVMNetSubnet(subnet)
		if err != nil {
			return darwinVZNetwork{}, err
		}
		if normalizedSubnet == "" {
			normalizedSubnet = darwinVZFileHandleDefaultSubnetCIDR
		}
		return darwinVZNetwork{
			Mode:       darwinVZNetworkModeFileHandle,
			SubnetCIDR: normalizedSubnet,
		}, nil
	default:
		return darwinVZNetwork{}, fmt.Errorf("unsupported darwin-vz network mode %q: only %q is supported", cfg.DarwinVZNetworkMode, darwinVZNetworkModeFileHandle)
	}
}

func normalizeDarwinVZVMNetSubnet(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", nil
	}

	prefix, err := netip.ParsePrefix(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse darwin-vz vmnet subnet %q: %w", trimmed, err)
	}
	prefix = prefix.Masked()
	if !prefix.Addr().Is4() {
		return "", fmt.Errorf("darwin-vz vmnet subnet %q must be an IPv4 CIDR", trimmed)
	}
	if prefix.Bits() > 30 {
		return "", fmt.Errorf("darwin-vz vmnet subnet %q must provide at least four addresses", trimmed)
	}
	if !isRFC1918Prefix(prefix) {
		return "", fmt.Errorf("darwin-vz vmnet subnet %q must be within an RFC1918 private range", trimmed)
	}
	return prefix.String(), nil
}

func isRFC1918Prefix(prefix netip.Prefix) bool {
	for _, privatePrefix := range darwinVZRFC1918Prefixes {
		if prefix.Bits() < privatePrefix.Bits() {
			continue
		}
		if privatePrefix.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}
