package darwinvz

import (
	"fmt"
	"strings"
)

const allowRulesIgnoredWarning = "darwin-vz ignores sandbox.network.allow entries; allowlist egress filtering is not implemented"

func evaluateNetworkPolicy(networkDefault string, allowCount int) (string, error) {
	if strings.TrimSpace(networkDefault) != "deny" {
		return "", fmt.Errorf("darwin-vz backend requires deny-by-default policy, got %q", networkDefault)
	}
	if allowCount > 0 {
		return allowRulesIgnoredWarning, nil
	}
	return "", nil
}
