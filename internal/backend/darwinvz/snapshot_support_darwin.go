//go:build darwin

package darwinvz

import "fmt"

type SnapshotSupport struct {
	Usable  bool
	Message string
}

var (
	detectSnapshotSupportResolveHelperPath = resolveHelperBinaryPath
	detectSnapshotSupportHasEntitlement    = helperHasVirtualizationEntitlement
)

func DetectSnapshotSupport() SnapshotSupport {
	helperPath, err := detectSnapshotSupportResolveHelperPath()
	if err != nil {
		return SnapshotSupport{Message: fmt.Sprintf("darwin-vz snapshots remain disabled: helper unavailable: %v", err)}
	}

	hasEntitlement, err := detectSnapshotSupportHasEntitlement(helperPath)
	if err != nil {
		return SnapshotSupport{Message: fmt.Sprintf("darwin-vz snapshots remain disabled: could not verify helper entitlement for %s: %v", helperPath, err)}
	}
	if !hasEntitlement {
		return SnapshotSupport{Message: fmt.Sprintf("darwin-vz snapshots remain disabled: %s is missing com.apple.security.virtualization entitlement", helperPath)}
	}

	return SnapshotSupport{
		Usable:  true,
		Message: fmt.Sprintf("darwin-vz snapshot runtime is usable via %s", helperPath),
	}
}
