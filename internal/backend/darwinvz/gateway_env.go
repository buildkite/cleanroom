package darwinvz

import (
	"strings"

	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/policy"
)

// gatewayRegistry is the subset of gateway.Registry used by the darwin
// adapter.
type gatewayRegistry interface {
	RegisterScopeToken(scopeToken, sandboxID string, p *policy.CompiledPolicy) error
	ReleaseScopeToken(scopeToken string)
}

func gatewayGitProxyEnvVars(compiled *policy.CompiledPolicy, networkMode string, gatewayPort int) []string {
	if !strings.EqualFold(strings.TrimSpace(networkMode), darwinVZNetworkModeFileHandle) {
		// darwin-vz only supports the file-handle guest gateway path.
		return nil
	}
	return gatewayEnvVars(compiled, gatewayPort, "")
}

func gatewayEnvVars(compiled *policy.CompiledPolicy, gatewayPort int, scopeToken string) []string {
	return gateway.GitProxyEnvVars(compiled, gatewayPort, scopeToken)
}
