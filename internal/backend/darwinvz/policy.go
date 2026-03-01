package darwinvz

import (
	"fmt"
	"strings"
)

const allowRulesIgnoredWarning = "darwin-vz ignores sandbox.network.allow entries; allowlist egress filtering is not implemented"
const allowRulesRequireFilterError = "darwin-vz requires host-side egress filtering for sandbox.network.allow entries; enable the Cleanroom network filter"
const guestNetworkUnavailableWarning = "darwin-vz guest networking is enabled without host-side egress filtering"
const guestNetworkProtectedMessage = "darwin-vz guest networking is protected by host-side egress filtering"

func evaluateNetworkPolicyForDoctor(networkDefault string, allowCount int, allowlistSupported bool) (string, error) {
	return evaluateNetworkPolicy(networkDefault, allowCount, allowlistSupported, false)
}

func evaluateNetworkPolicyForRun(networkDefault string, allowCount int, allowlistSupported bool) (string, error) {
	return evaluateNetworkPolicy(networkDefault, allowCount, allowlistSupported, true)
}

func evaluateNetworkPolicy(networkDefault string, allowCount int, allowlistSupported, requireAllowlistEnforcement bool) (string, error) {
	if strings.TrimSpace(networkDefault) != "deny" {
		return "", fmt.Errorf("darwin-vz backend requires deny-by-default policy, got %q", networkDefault)
	}
	if allowCount > 0 && !allowlistSupported {
		if requireAllowlistEnforcement {
			return "", fmt.Errorf("%s", allowRulesRequireFilterError)
		}
		return allowRulesIgnoredWarning, nil
	}
	return "", nil
}
