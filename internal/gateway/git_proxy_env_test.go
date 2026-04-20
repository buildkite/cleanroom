package gateway

import (
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

func TestRubyGemsProxyEnvVarsRequiresLiveRoute(t *testing.T) {
	t.Parallel()

	compiled := &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow:          []policy.AllowRule{{Host: "rubygems.org", Ports: []int{443}}},
	}

	if env := RubyGemsProxyEnvVars(compiled, 8170, ProxyRoutes{}); env != nil {
		t.Fatalf("expected no rubygems proxy env without live route, got %v", env)
	}

	env := RubyGemsProxyEnvVars(compiled, 8170, ProxyRoutes{RubyGems: true})
	if len(env) != 3 {
		t.Fatalf("expected 3 env vars with live route, got %d: %v", len(env), env)
	}
}

func TestProxyEnvVarsStillIncludesGitWhenRubyGemsRouteIsUnavailable(t *testing.T) {
	t.Parallel()

	compiled := &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow: []policy.AllowRule{
			{Host: "github.com", Ports: []int{443}},
			{Host: "rubygems.org", Ports: []int{443}},
		},
	}

	env := ProxyEnvVars(compiled, 8170, "", ProxyRoutes{})
	if len(env) != 5 {
		t.Fatalf("expected only git env vars when rubygems route is unavailable, got %d: %v", len(env), env)
	}
	if env[0] != "GIT_CONFIG_COUNT=2" {
		t.Fatalf("expected git config count only, got %q", env[0])
	}
	for _, entry := range env {
		if entry == BundlerAppConfigEnvKey+"="+BundlerAppConfigPath {
			t.Fatalf("did not expect bundler app config env when rubygems route is unavailable: %v", env)
		}
		if entry == BundlerRubyGemsFallbackTimeoutEnvKey+"=0" {
			t.Fatalf("did not expect bundler fallback timeout env when rubygems route is unavailable: %v", env)
		}
		if entry == BundlerRubyGemsMirrorEnvKey+"=http://"+GuestGatewayHostname+":8170/rubygems/" {
			t.Fatalf("did not expect rubygems mirror env when rubygems route is unavailable: %v", env)
		}
	}
}

func TestGoProxyEnvVarsRequiresLiveRoute(t *testing.T) {
	t.Parallel()

	compiled := &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow:          []policy.AllowRule{{Host: "proxy.golang.org", Ports: []int{443}}},
	}

	if env := GoProxyEnvVars(compiled, 8170, ProxyRoutes{}); env != nil {
		t.Fatalf("expected no goproxy env without live route, got %v", env)
	}

	env := GoProxyEnvVars(compiled, 8170, ProxyRoutes{GoProxy: true})
	if len(env) != 1 {
		t.Fatalf("expected 1 goproxy env var with live route, got %d: %v", len(env), env)
	}
	if want := "GOPROXY=http://" + GuestGatewayHostname + ":8170/goproxy,direct"; env[0] != want {
		t.Fatalf("expected %q, got %q", want, env[0])
	}
}

func TestMiseProxyEnvVarsRequiresLiveRoute(t *testing.T) {
	t.Parallel()

	compiled := &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow:          []policy.AllowRule{{Host: "dl.google.com", Ports: []int{443}}},
	}

	if env := MiseProxyEnvVars(compiled, 8170, ProxyRoutes{}); env != nil {
		t.Fatalf("expected no mise fetch env without live route, got %v", env)
	}

	env := MiseProxyEnvVars(compiled, 8170, ProxyRoutes{Fetch: true})
	if len(env) != 1 {
		t.Fatalf("expected 1 mise fetch env var with live route, got %d: %v", len(env), env)
	}
	if want := "MISE_GO_DOWNLOAD_MIRROR=http://" + GuestGatewayHostname + ":8170/fetch/dl.google.com/go"; env[0] != want {
		t.Fatalf("expected %q, got %q", want, env[0])
	}
}
