package controlservice

import (
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

func resolveBackendName(requested, configuredDefault string) string {
	if requested != "" {
		return requested
	}
	if configuredDefault != "" {
		return configuredDefault
	}
	return runtimeconfig.DefaultBackendForHost()
}
