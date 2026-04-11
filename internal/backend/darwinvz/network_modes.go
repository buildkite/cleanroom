package darwinvz

import (
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
)

const (
	darwinVZNetworkModeFileHandle = "filehandle"
)

func darwinVZConfiguredOrDefaultNetworkMode(mode string) string {
	normalized := strings.ToLower(strings.TrimSpace(mode))
	if normalized == "" {
		return darwinVZNetworkModeFileHandle
	}
	return normalized
}

func validateDarwinVZNetworkSelection(cfg backend.FirecrackerConfig) error {
	if darwinVZConfiguredOrDefaultNetworkMode(cfg.DarwinVZNetworkMode) != darwinVZNetworkModeFileHandle {
		return fmt.Errorf("unsupported darwin-vz network mode %q: only %q is supported", cfg.DarwinVZNetworkMode, darwinVZNetworkModeFileHandle)
	}
	return errForRemovedDarwinVZNetworkSettings(
		strings.TrimSpace(cfg.DarwinVZNetworkExternalInterface),
		cfg.DarwinVZNetworkDisableNAT44,
		cfg.DarwinVZNetworkDisableNAT66,
		cfg.DarwinVZNetworkDisableDNSProxy,
		cfg.DarwinVZNetworkDisableRouterAdvertisement,
	)
}

func errForRemovedDarwinVZNetworkSettings(externalInterface string, disableNAT44, disableNAT66, disableDNSProxy, disableRouterAdvertisement bool) error {
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
	if len(removedSettings) == 0 {
		return nil
	}
	return fmt.Errorf("darwin-vz no longer supports legacy vmnet settings: %s", strings.Join(removedSettings, ", "))
}
