//go:build darwin

package darwinvz

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"golang.org/x/sys/unix"
)

const (
	darwinVZNetworkModeNAT         = "nat"
	darwinVZNetworkModeVMNetShared = "vmnet-shared"
)

type darwinVZNetwork struct {
	Mode       string
	SubnetCIDR string
}

var darwinVZRFC1918Prefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

var darwinVZVMNetSharedSupported = hostSupportsVMNetShared

func hostSupportsVMNetShared() bool {
	version, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return false
	}
	parts := strings.SplitN(strings.TrimSpace(version), ".", 2)
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	return major >= 26
}

func darwinVZDefaultNetworkMode() string {
	if darwinVZVMNetSharedSupported() {
		return darwinVZNetworkModeVMNetShared
	}
	return darwinVZNetworkModeNAT
}

func resolveDarwinVZNetwork(cfg backend.FirecrackerConfig) (darwinVZNetwork, error) {
	mode := strings.ToLower(strings.TrimSpace(cfg.DarwinVZNetworkMode))
	if mode == "" {
		mode = darwinVZDefaultNetworkMode()
	}

	subnet := strings.TrimSpace(cfg.DarwinVZNetworkSubnet)
	switch mode {
	case darwinVZNetworkModeNAT:
		if subnet != "" {
			return darwinVZNetwork{}, fmt.Errorf("darwin-vz network subnet requires %q mode", darwinVZNetworkModeVMNetShared)
		}
		return darwinVZNetwork{Mode: darwinVZNetworkModeNAT}, nil
	case darwinVZNetworkModeVMNetShared:
		if !darwinVZVMNetSharedSupported() {
			return darwinVZNetwork{}, fmt.Errorf("%q requires macOS 26 or later", darwinVZNetworkModeVMNetShared)
		}
		normalizedSubnet, err := normalizeDarwinVZVMNetSubnet(subnet)
		if err != nil {
			return darwinVZNetwork{}, err
		}
		return darwinVZNetwork{Mode: darwinVZNetworkModeVMNetShared, SubnetCIDR: normalizedSubnet}, nil
	default:
		return darwinVZNetwork{}, fmt.Errorf("unsupported darwin-vz network mode %q", cfg.DarwinVZNetworkMode)
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
