package gateway

import (
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/policy"
)

// GuestGatewayHostname is the canonical guest-visible hostname for the shared
// Cleanroom host gateway.
const GuestGatewayHostname = "gateway.cleanroom.internal"

const (
	// BundlerRubyGemsMirrorEnvKey is a shell-safe hint consumed by the guest
	// agent, which expands it into Bundler's mirror config env keys before
	// launching the requested process.
	BundlerRubyGemsMirrorEnvKey = "CLEANROOM_BUNDLER_RUBYGEMS_MIRROR"
	// BundlerRubyGemsFallbackTimeoutEnvKey carries Bundler's mirror fallback
	// timeout setting through the control plane using a shell-safe key.
	BundlerRubyGemsFallbackTimeoutEnvKey = "CLEANROOM_BUNDLER_RUBYGEMS_FALLBACK_TIMEOUT"
	// BundlerAppConfigEnvKey points Bundler at a shell-safe config directory we
	// control inside the guest.
	BundlerAppConfigEnvKey = "BUNDLE_APP_CONFIG"
	// BundlerAppConfigPath is the guest path used for generated Bundler config.
	BundlerAppConfigPath = "/tmp/cleanroom-bundle-config"
	// GoProxyDefaultEnvKey is a shell-safe hint consumed by the guest agent to
	// set GOPROXY only when the caller has not already defined it.
	GoProxyDefaultEnvKey = "CLEANROOM_GOPROXY_DEFAULT"
	// MiseGoDownloadMirrorDefaultEnvKey is a shell-safe hint consumed by the
	// guest agent to set MISE_GO_DOWNLOAD_MIRROR only when the caller has not
	// already defined it.
	MiseGoDownloadMirrorDefaultEnvKey = "CLEANROOM_MISE_GO_DOWNLOAD_MIRROR_DEFAULT"
)

// ProxyRoutes describes which gateway-backed guest proxy routes are live.
type ProxyRoutes struct {
	RubyGems bool
	GoProxy  bool
	Fetch    bool
}

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

// RubyGemsProxyEnvVars returns Bundler config environment variables that mirror
// RubyGems traffic through the shared host gateway when rubygems.org is
// policy-allowed.
func RubyGemsProxyEnvVars(compiled *policy.CompiledPolicy, gatewayPort int, routes ProxyRoutes) []string {
	if !routes.RubyGems || !allowsHTTPSHost(compiled, "rubygems.org") || gatewayPort <= 0 {
		return nil
	}

	gatewayAddr := fmt.Sprintf("http://%s:%d", GuestGatewayHostname, gatewayPort)
	return []string{
		fmt.Sprintf("%s=%s", BundlerAppConfigEnvKey, BundlerAppConfigPath),
		fmt.Sprintf("%s=%s/rubygems/", BundlerRubyGemsMirrorEnvKey, gatewayAddr),
		BundlerRubyGemsFallbackTimeoutEnvKey + "=0",
	}
}

// GoProxyEnvVars returns environment variables that route Go module downloads
// through the shared host gateway when proxy.golang.org is policy-allowed.
func GoProxyEnvVars(compiled *policy.CompiledPolicy, gatewayPort int, routes ProxyRoutes) []string {
	if !routes.GoProxy || !allowsHTTPSHost(compiled, "proxy.golang.org") || !allowsHTTPSHost(compiled, "sum.golang.org") || gatewayPort <= 0 {
		return nil
	}

	return []string{
		fmt.Sprintf("%s=http://%s:%d/goproxy,direct", GoProxyDefaultEnvKey, GuestGatewayHostname, gatewayPort),
	}
}

// MiseProxyEnvVars returns environment variables that route immutable mise Go
// SDK downloads through the shared host gateway when dl.google.com is
// policy-allowed.
func MiseProxyEnvVars(compiled *policy.CompiledPolicy, gatewayPort int, routes ProxyRoutes) []string {
	if !routes.Fetch || !allowsHTTPSHost(compiled, "dl.google.com") || gatewayPort <= 0 {
		return nil
	}

	return []string{
		fmt.Sprintf("%s=http://%s:%d/fetch/dl.google.com/go", MiseGoDownloadMirrorDefaultEnvKey, GuestGatewayHostname, gatewayPort),
	}
}

// ProxyEnvVars returns gateway-specific environment variables for guest
// processes, including Git rewrite config, Bundler RubyGems mirroring, Go
// module proxying, and immutable tool download mirroring.
func ProxyEnvVars(compiled *policy.CompiledPolicy, gatewayPort int, scopeToken string, routes ProxyRoutes) []string {
	env := GitProxyEnvVars(compiled, gatewayPort, scopeToken)
	env = append(env, RubyGemsProxyEnvVars(compiled, gatewayPort, routes)...)
	env = append(env, GoProxyEnvVars(compiled, gatewayPort, routes)...)
	env = append(env, MiseProxyEnvVars(compiled, gatewayPort, routes)...)
	if len(env) == 0 {
		return nil
	}
	return env
}

func allowsHTTPSHost(compiled *policy.CompiledPolicy, host string) bool {
	return compiled != nil && compiled.Allows(strings.TrimSpace(host), 443)
}
