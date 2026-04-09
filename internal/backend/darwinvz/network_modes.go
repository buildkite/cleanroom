package darwinvz

import "strings"

const (
	darwinVZNetworkModeNAT         = "nat"
	darwinVZNetworkModeFileHandle  = "filehandle"
	darwinVZNetworkModeVMNetShared = "vmnet-shared"
)

func darwinVZConfiguredOrDefaultNetworkMode(mode string) string {
	normalized := strings.ToLower(strings.TrimSpace(mode))
	if normalized == "" {
		return darwinVZNetworkModeFileHandle
	}
	return normalized
}
