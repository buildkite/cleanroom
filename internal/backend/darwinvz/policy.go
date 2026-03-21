package darwinvz

import (
	"fmt"
	"strings"
)

const allowRulesIgnoredWarning = "darwin-vz ignores sandbox.network.allow entries; allowlist egress filtering is not implemented"
const guestNetworkUnavailableWarning = "darwin-vz guest networking is enabled without host-side egress filtering"
const allowAllPolicyWarning = "darwin-vz allows outbound networking for network.default=allow; host-side egress filtering is disabled"

func evaluateNetworkPolicy(networkDefault string, allowCount int) (string, error) {
	switch strings.TrimSpace(networkDefault) {
	case "deny":
		if allowCount > 0 {
			return allowRulesIgnoredWarning, nil
		}
		return "", nil
	case "allow":
		if allowCount > 0 {
			return allowAllPolicyWarning + "; sandbox.network.allow entries are ignored", nil
		}
		return allowAllPolicyWarning, nil
	default:
		return "", fmt.Errorf("darwin-vz backend requires network.default=deny or allow, got %q", networkDefault)
	}
}
