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

const darwinVZFileHandleDefaultSubnetCIDR = "10.233.0.0/24"

type darwinVZNetwork struct {
	Mode                       string
	SubnetCIDR                 string
	ExternalInterface          string
	DisableNAT44               bool
	DisableNAT66               bool
	DisableDNSProxy            bool
	DisableRouterAdvertisement bool
}

var darwinVZRFC1918Prefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

var (
	darwinVZVMNetSharedSupported                  = hostSupportsVMNetShared
	resolveDarwinVZNetworkHelperPath              = resolveHelperBinaryPath
	helperHasVMNetworkingEntitlementForNetworking = helperHasVMNetworkingEntitlement
)

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
	if !darwinVZVMNetSharedSupported() {
		return darwinVZNetworkModeNAT
	}

	helperPath, err := resolveDarwinVZNetworkHelperPath()
	if err != nil {
		return darwinVZNetworkModeNAT
	}
	hasVMNetEntitlement, err := helperHasVMNetworkingEntitlementForNetworking(helperPath)
	if err != nil || !hasVMNetEntitlement {
		return darwinVZNetworkModeNAT
	}

	return darwinVZNetworkModeVMNetShared
}

func resolveDarwinVZNetwork(cfg backend.FirecrackerConfig) (darwinVZNetwork, error) {
	mode := strings.ToLower(strings.TrimSpace(cfg.DarwinVZNetworkMode))
	if mode == "" {
		mode = darwinVZDefaultNetworkMode()
	}

	subnet := strings.TrimSpace(cfg.DarwinVZNetworkSubnet)
	externalInterface := strings.TrimSpace(cfg.DarwinVZNetworkExternalInterface)
	disableNAT44 := cfg.DarwinVZNetworkDisableNAT44
	disableNAT66 := cfg.DarwinVZNetworkDisableNAT66
	disableDNSProxy := cfg.DarwinVZNetworkDisableDNSProxy
	disableRouterAdvertisement := cfg.DarwinVZNetworkDisableRouterAdvertisement
	hasVMNetOnlySettings := externalInterface != "" || disableNAT44 || disableNAT66 || disableDNSProxy || disableRouterAdvertisement
	switch mode {
	case darwinVZNetworkModeNAT:
		if subnet != "" {
			return darwinVZNetwork{}, fmt.Errorf("darwin-vz custom network subnet requires %q or %q mode", darwinVZNetworkModeVMNetShared, darwinVZNetworkModeFileHandle)
		}
		if hasVMNetOnlySettings {
			return darwinVZNetwork{}, fmt.Errorf("darwin-vz vmnet network settings require %q mode", darwinVZNetworkModeVMNetShared)
		}
		return darwinVZNetwork{Mode: darwinVZNetworkModeNAT}, nil
	case darwinVZNetworkModeFileHandle:
		if hasVMNetOnlySettings {
			return darwinVZNetwork{}, fmt.Errorf("darwin-vz vmnet network settings require %q mode", darwinVZNetworkModeVMNetShared)
		}
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
	case darwinVZNetworkModeVMNetShared:
		if !darwinVZVMNetSharedSupported() {
			return darwinVZNetwork{}, fmt.Errorf("%q requires macOS 26 or later", darwinVZNetworkModeVMNetShared)
		}
		normalizedSubnet, err := normalizeDarwinVZVMNetSubnet(subnet)
		if err != nil {
			return darwinVZNetwork{}, err
		}
		return darwinVZNetwork{
			Mode:                       darwinVZNetworkModeVMNetShared,
			SubnetCIDR:                 normalizedSubnet,
			ExternalInterface:          externalInterface,
			DisableNAT44:               disableNAT44,
			DisableNAT66:               disableNAT66,
			DisableDNSProxy:            disableDNSProxy,
			DisableRouterAdvertisement: disableRouterAdvertisement,
		}, nil
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
