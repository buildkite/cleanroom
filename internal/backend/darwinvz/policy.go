package darwinvz

import (
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
)

const guestNetworkProtectedMessage = "darwin-vz guest networking is protected by host-side egress filtering"
const guestNetworkProtectedByFileHandleMessage = "darwin-vz guest networking is protected by file-handle gateway filtering"
const guestNetworkUnavailableWarning = "darwin-vz guest networking is enabled without host-side egress filtering"

func evaluateNetworkPolicyForDoctor(networkDefault string, allowCount int, allowlistSupported bool) (string, error) {
	return evaluateNetworkPolicy(networkDefault, allowCount, allowlistSupported, false)
}

func evaluateNetworkPolicyForRun(networkDefault string, allowCount int, allowlistSupported bool) (string, error) {
	return evaluateNetworkPolicy(networkDefault, allowCount, allowlistSupported, true)
}

func evaluateNetworkPolicy(networkDefault string, allowCount int, allowlistSupported, requireAllowlistEnforcement bool) (string, error) {
	_, _ = allowCount, requireAllowlistEnforcement
	if strings.TrimSpace(networkDefault) != "deny" {
		return "", fmt.Errorf("darwin-vz backend requires deny-by-default policy, got %q", networkDefault)
	}
	return "", nil
}

func allowlistSupportForConfig(cfg backend.FirecrackerConfig) (supported bool, detail, protectionMessage string, err error) {
	networkCfg, err := resolveDarwinVZNetwork(cfg)
	if err != nil {
		return false, "", "", err
	}
	if networkCfg.Mode == darwinVZNetworkModeFileHandle {
		return true, "", guestNetworkProtectedByFileHandleMessage, nil
	}
	return true, "", guestNetworkProtectedMessage, nil
}
