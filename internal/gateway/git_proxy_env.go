package gateway

import (
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/policy"
)

// GuestGatewayHostname is the canonical guest-visible hostname for the shared
// Cleanroom host gateway.
const GuestGatewayHostname = "gateway.cleanroom.internal"

// GitProxyEnvVars returns git config environment variables that rewrite allowed
// HTTPS remotes through the shared host gateway.
func GitProxyEnvVars(compiled *policy.CompiledPolicy, gatewayPort int, scopeToken string) []string {
	if compiled == nil || gatewayPort <= 0 {
		return nil
	}

	type configEntry struct {
		key   string
		value string
	}

	gatewayAddr := fmt.Sprintf("http://%s:%d", GuestGatewayHostname, gatewayPort)
	entries := make([]configEntry, 0, len(compiled.Allow)+1)
	seenHosts := make(map[string]struct{}, len(compiled.Allow))
	for _, rule := range compiled.Allow {
		host := strings.TrimSpace(rule.Host)
		if host == "" {
			continue
		}
		for _, port := range rule.Ports {
			if port != 443 {
				continue
			}
			if _, exists := seenHosts[host]; exists {
				break
			}
			seenHosts[host] = struct{}{}
			entries = append(entries, configEntry{
				key:   fmt.Sprintf("url.%s/git/%s/.insteadOf", gatewayAddr, host),
				value: fmt.Sprintf("https://%s/", host),
			})
			break
		}
	}
	if len(entries) == 0 {
		return nil
	}

	scopeToken = strings.TrimSpace(scopeToken)
	if scopeToken != "" {
		entries = append(entries, configEntry{
			key:   fmt.Sprintf("http.%s/.extraHeader", gatewayAddr),
			value: fmt.Sprintf("%s: %s", ScopeTokenHeader, scopeToken),
		})
	}

	env := make([]string, 0, 1+len(entries)*2)
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(entries)))
	for i, entry := range entries {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, entry.key))
		env = append(env, fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, entry.value))
	}
	return env
}
