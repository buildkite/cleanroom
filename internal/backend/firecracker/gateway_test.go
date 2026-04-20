package firecracker

import (
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/policy"
)

func TestGatewayEnvVarsEmptyWhenNoPolicyHosts(t *testing.T) {
	t.Parallel()

	instance := &sandboxInstance{
		Policy: &policy.CompiledPolicy{
			Version:        1,
			NetworkDefault: "deny",
			Allow:          []policy.AllowRule{{Host: "example.com", Ports: []int{80}}},
		},
	}
	env := gatewayEnvVars(instance, 8170, gateway.ProxyRoutes{RubyGems: true})
	if len(env) != 0 {
		t.Fatalf("expected no env vars for host without port 443, got %v", env)
	}
}

func TestGatewayEnvVarsGeneratesGitConfig(t *testing.T) {
	t.Parallel()

	instance := &sandboxInstance{
		Policy: &policy.CompiledPolicy{
			Version:        1,
			NetworkDefault: "deny",
			Allow: []policy.AllowRule{
				{Host: "github.com", Ports: []int{443}},
				{Host: "gitlab.com", Ports: []int{443}},
			},
		},
	}

	env := gatewayEnvVars(instance, 8170, gateway.ProxyRoutes{})
	if len(env) != 5 {
		t.Fatalf("expected 5 env vars (1 count + 2*2 key/value), got %d: %v", len(env), env)
	}

	if env[0] != "GIT_CONFIG_COUNT=2" {
		t.Fatalf("expected GIT_CONFIG_COUNT=2, got %s", env[0])
	}

	if !strings.Contains(env[1], "url.http://"+gateway.GuestGatewayHostname+":8170/git/github.com/.insteadOf") {
		t.Fatalf("expected github.com insteadOf key, got %s", env[1])
	}
	if env[2] != "GIT_CONFIG_VALUE_0=https://github.com/" {
		t.Fatalf("expected github.com value, got %s", env[2])
	}
	if !strings.Contains(env[3], "url.http://"+gateway.GuestGatewayHostname+":8170/git/gitlab.com/.insteadOf") {
		t.Fatalf("expected gitlab.com insteadOf key, got %s", env[3])
	}
}

func TestGatewayEnvVarsNilPolicy(t *testing.T) {
	t.Parallel()

	instance := &sandboxInstance{}
	env := gatewayEnvVars(instance, 8170, gateway.ProxyRoutes{RubyGems: true})
	if env != nil {
		t.Fatalf("expected nil for nil policy, got %v", env)
	}
}

func TestGatewayEnvVarsSkipsRubyGemsMirrorWithoutLiveRoute(t *testing.T) {
	t.Parallel()

	instance := &sandboxInstance{
		Policy: &policy.CompiledPolicy{
			Version:        1,
			NetworkDefault: "deny",
			Allow:          []policy.AllowRule{{Host: "rubygems.org", Ports: []int{443}}},
		},
	}

	env := gatewayEnvVars(instance, 8170, gateway.ProxyRoutes{})
	for _, entry := range env {
		if entry == gateway.BundlerAppConfigEnvKey+"="+gateway.BundlerAppConfigPath {
			t.Fatalf("did not expect bundler app config env without a live rubygems route, got %v", env)
		}
		if entry == gateway.BundlerRubyGemsFallbackTimeoutEnvKey+"=0" {
			t.Fatalf("did not expect bundler fallback timeout env without a live rubygems route, got %v", env)
		}
		if entry == gateway.BundlerRubyGemsMirrorEnvKey+"=http://"+gateway.GuestGatewayHostname+":8170/rubygems/" {
			t.Fatalf("did not expect rubygems mirror env without a live rubygems route, got %v", env)
		}
	}
}

func TestGatewayEnvVarsAddsGoProxyAndMiseMirror(t *testing.T) {
	t.Parallel()

	instance := &sandboxInstance{
		Policy: &policy.CompiledPolicy{
			Version:        1,
			NetworkDefault: "deny",
			Allow: []policy.AllowRule{
				{Host: "proxy.golang.org", Ports: []int{443}},
				{Host: "dl.google.com", Ports: []int{443}},
			},
		},
	}

	env := gatewayEnvVars(instance, 8170, gateway.ProxyRoutes{GoProxy: true, Fetch: true})
	foundGoProxy := false
	foundMiseMirror := false
	for _, entry := range env {
		if entry == "GOPROXY=http://"+gateway.GuestGatewayHostname+":8170/goproxy,direct" {
			foundGoProxy = true
		}
		if entry == "MISE_GO_DOWNLOAD_MIRROR=http://"+gateway.GuestGatewayHostname+":8170/fetch/dl.google.com/go" {
			foundMiseMirror = true
		}
	}
	if !foundGoProxy {
		t.Fatalf("expected GOPROXY env in %v", env)
	}
	if !foundMiseMirror {
		t.Fatalf("expected MISE_GO_DOWNLOAD_MIRROR env in %v", env)
	}
}
