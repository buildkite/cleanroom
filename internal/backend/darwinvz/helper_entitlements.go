//go:build darwin

package darwinvz

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var (
	virtualizationEntitlementPattern = regexp.MustCompile(`(?s)<key>\s*com\.apple\.security\.virtualization\s*</key>\s*<true\s*/?>`)
	vmNetworkingEntitlementPattern   = regexp.MustCompile(`(?s)<key>\s*com\.apple\.developer\.networking\.vmnet\s*</key>\s*<true\s*/?>`)
)

func helperHasVirtualizationEntitlement(helperPath string) (bool, error) {
	return helperHasEntitlement(helperPath, virtualizationEntitlementPattern)
}

func helperHasVMNetworkingEntitlement(helperPath string) (bool, error) {
	return helperHasEntitlement(helperPath, vmNetworkingEntitlementPattern)
}

func doctorVMNetEntitlementResult(helperPath string, hasEntitlement bool, entitlementErr error) (string, string) {
	switch {
	case entitlementErr != nil:
		return "warn", fmt.Sprintf(
			"could not verify com.apple.developer.networking.vmnet entitlement on %s: %v",
			helperPath,
			entitlementErr,
		)
	case hasEntitlement:
		return "pass", fmt.Sprintf("%s includes com.apple.developer.networking.vmnet entitlement", helperPath)
	default:
		return "warn", fmt.Sprintf(
			"%s does not declare com.apple.developer.networking.vmnet; unsandboxed local builds may still work with vmnet-shared, but signed helpers and CI should use cmd/cleanroom-darwin-vz/entitlements-vmnet.plist and the matching provisioning profile or identifier",
			helperPath,
		)
	}
}

func helperHasEntitlement(helperPath string, pattern *regexp.Regexp) (bool, error) {
	entitlements, err := readHelperEntitlements(helperPath)
	if err != nil {
		return false, err
	}
	return pattern.MatchString(entitlements), nil
}

func readHelperEntitlements(helperPath string) (string, error) {
	cmd := exec.Command("codesign", "-d", "--entitlements", ":-", helperPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(output))
		if msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return normalizeHelperEntitlements(string(output)), nil
}

func hasVirtualizationEntitlement(raw string) bool {
	return virtualizationEntitlementPattern.MatchString(normalizeHelperEntitlements(raw))
}

func hasVMNetworkingEntitlement(raw string) bool {
	return vmNetworkingEntitlementPattern.MatchString(normalizeHelperEntitlements(raw))
}

func normalizeHelperEntitlements(raw string) string {
	entitlements := strings.TrimSpace(raw)
	if entitlements == "" {
		return ""
	}

	if start := strings.Index(entitlements, "<?xml"); start >= 0 {
		return entitlements[start:]
	}
	if start := strings.Index(entitlements, "<plist"); start >= 0 {
		return entitlements[start:]
	}
	return entitlements
}
