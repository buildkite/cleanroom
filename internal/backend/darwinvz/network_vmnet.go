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
	mode := darwinVZConfiguredOrDefaultNetworkMode(cfg.DarwinVZNetworkMode)

	subnet := strings.TrimSpace(cfg.DarwinVZNetworkSubnet)
	externalInterface := strings.TrimSpace(cfg.DarwinVZNetworkExternalInterface)
	disableNAT44 := cfg.DarwinVZNetworkDisableNAT44
	disableNAT66 := cfg.DarwinVZNetworkDisableNAT66
	disableDNSProxy := cfg.DarwinVZNetworkDisableDNSProxy
	disableRouterAdvertisement := cfg.DarwinVZNetworkDisableRouterAdvertisement
	hasRemovedLegacySettings := externalInterface != "" || disableNAT44 || disableNAT66 || disableDNSProxy || disableRouterAdvertisement
	switch mode {
	case darwinVZNetworkModeFileHandle:
		if hasRemovedLegacySettings {
			return darwinVZNetwork{}, errorsForRemovedDarwinVZNetworkSettings(externalInterface, disableNAT44, disableNAT66, disableDNSProxy, disableRouterAdvertisement)
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
	default:
		return darwinVZNetwork{}, fmt.Errorf("unsupported darwin-vz network mode %q: only %q is supported", cfg.DarwinVZNetworkMode, darwinVZNetworkModeFileHandle)
	}
}

func errorsForRemovedDarwinVZNetworkSettings(externalInterface string, disableNAT44, disableNAT66, disableDNSProxy, disableRouterAdvertisement bool) error {
	removedSettings := make([]string, 0, 5)
	if externalInterface != "" {
		removedSettings = append(removedSettings, "external_interface")
	}
	if disableNAT44 {
		removedSettings = append(removedSettings, "disable_nat44")
	}
	if disableNAT66 {
		removedSettings = append(removedSettings, "disable_nat66")
	}
	if disableDNSProxy {
		removedSettings = append(removedSettings, "disable_dns_proxy")
	}
	if disableRouterAdvertisement {
		removedSettings = append(removedSettings, "disable_router_advertisement")
	}
	return fmt.Errorf("darwin-vz no longer supports legacy vmnet settings: %s", strings.Join(removedSettings, ", "))
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
