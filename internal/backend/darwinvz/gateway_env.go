package darwinvz

import (
	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/gatewayauth"
	"github.com/buildkite/cleanroom/internal/policy"
	"go.opentelemetry.io/otel/trace"
)

// gatewayRegistry is the subset of gateway.Registry used by the darwin
// adapter.
type gatewayRegistry interface {
	RegisterScopeToken(scopeToken, sandboxID string, p *policy.CompiledPolicy, metadata ...gatewayauth.ScopeMetadata) error
	ReleaseScopeToken(scopeToken string)
	SetActiveExecutionTrace(sandboxID, executionID string, spanContext trace.SpanContext)
	ClearActiveExecutionTrace(sandboxID, executionID string)
}

func gatewayGitProxyEnvVars(compiled *policy.CompiledPolicy, networkMode string, gatewayPort int, routes gateway.ProxyRoutes) []string {
	if darwinVZConfiguredOrDefaultNetworkMode(networkMode) != darwinVZNetworkModeFileHandle {
		// darwin-vz only supports the file-handle guest gateway path.
		return nil
	}
	return gatewayEnvVars(compiled, gatewayPort, "", routes)
}

func gatewayEnvVars(compiled *policy.CompiledPolicy, gatewayPort int, scopeToken string, routes gateway.ProxyRoutes) []string {
	return gateway.ProxyEnvVars(compiled, gatewayPort, scopeToken, routes)
}
