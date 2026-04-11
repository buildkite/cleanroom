package darwinvz

import "strings"

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
