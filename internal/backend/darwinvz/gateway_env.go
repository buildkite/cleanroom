package darwinvz

import (
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/policy"
)

const defaultGatewayHost = "192.168.64.1"

// gatewayRegistry is the subset of gateway.Registry used by the darwin
// adapter.
type gatewayRegistry interface {
	RegisterScopeToken(scopeToken, sandboxID string, p *policy.CompiledPolicy) error
	ReleaseScopeToken(scopeToken string)
}

func gatewayEnvVars(compiled *policy.CompiledPolicy, gatewayHost string, gatewayPort int, scopeToken string) []string {
	if compiled == nil {
		return nil
	}
	if gatewayPort <= 0 {
		return nil
	}
	gatewayHost = strings.TrimSpace(gatewayHost)
	scopeToken = strings.TrimSpace(scopeToken)
	if gatewayHost == "" || scopeToken == "" {
		return nil
	}

	type configEntry struct {
		key   string
		value string
	}

	entries := make([]configEntry, 0, len(compiled.Allow)+1)
	for _, rule := range compiled.Allow {
		host := strings.TrimSpace(rule.Host)
		if host == "" {
			continue
		}
		for _, port := range rule.Ports {
			if port != 443 {
				continue
			}
			entries = append(entries, configEntry{
				key:   fmt.Sprintf("url.http://%s:%d/git/%s/.insteadOf", gatewayHost, gatewayPort, host),
				value: fmt.Sprintf("https://%s/", host),
			})
			break
		}
	}

	if len(entries) == 0 {
		return nil
	}

	gatewayAddr := fmt.Sprintf("http://%s:%d", gatewayHost, gatewayPort)
	entries = append(entries, configEntry{
		key:   fmt.Sprintf("http.%s/.extraHeader", gatewayAddr),
		value: fmt.Sprintf("%s: %s", gateway.ScopeTokenHeader, scopeToken),
	})

	env := make([]string, 0, 1+len(entries)*2)
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(entries)))
	for i, entry := range entries {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, entry.key))
		env = append(env, fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, entry.value))
	}
	return env
}
