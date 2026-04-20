package darwinvz

import (
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/policy"
)

func TestGatewayEnvVarsNilPolicy(t *testing.T) {
	t.Parallel()
	env := gatewayEnvVars(nil, 8170, "token", gateway.ProxyRoutes{RubyGems: true})
	if env != nil {
		t.Fatalf("expected nil env for nil policy, got %v", env)
	}
}

func TestGatewayEnvVarsNoHTTPSHosts(t *testing.T) {
	t.Parallel()
	p := &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow:          []policy.AllowRule{{Host: "example.com", Ports: []int{80}}},
	}
	env := gatewayEnvVars(p, 8170, "token", gateway.ProxyRoutes{RubyGems: true})
	if env != nil {
		t.Fatalf("expected nil env without port 443 hosts, got %v", env)
	}
}

func TestGatewayEnvVarsAddsRubyGemsMirror(t *testing.T) {
	t.Parallel()

	p := &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow:          []policy.AllowRule{{Host: "rubygems.org", Ports: []int{443}}},
	}
	env := gatewayEnvVars(p, 8170, "token", gateway.ProxyRoutes{RubyGems: true})
	wantAppConfig := gateway.BundlerAppConfigEnvKey + "=" + gateway.BundlerAppConfigPath
	wantMirror := gateway.BundlerRubyGemsMirrorEnvKey + "=http://" + gateway.GuestGatewayHostname + ":8170/rubygems/"
	foundAppConfig := false
	foundMirror := false
	foundFallback := false
	for _, entry := range env {
		if entry == wantAppConfig {
			foundAppConfig = true
		}
		if entry == wantMirror {
			foundMirror = true
		}
		if entry == gateway.BundlerRubyGemsFallbackTimeoutEnvKey+"=0" {
			foundFallback = true
		}
	}
	if !foundAppConfig {
		t.Fatalf("expected Bundler app config env %q in %v", wantAppConfig, env)
	}
	if !foundMirror {
		t.Fatalf("expected rubyGems mirror env %q in %v", wantMirror, env)
	}
	if !foundFallback {
		t.Fatalf("expected RubyGems fallback timeout env in %v", env)
	}
}

func TestGatewayEnvVarsSkipsRubyGemsMirrorWithoutLiveRoute(t *testing.T) {
	t.Parallel()

	p := &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow:          []policy.AllowRule{{Host: "rubygems.org", Ports: []int{443}}},
	}

	env := gatewayEnvVars(p, 8170, "token", gateway.ProxyRoutes{})
	for _, entry := range env {
		if entry == gateway.BundlerAppConfigEnvKey+"="+gateway.BundlerAppConfigPath {
			t.Fatalf("did not expect bundler app config env without live rubygems route, got %v", env)
		}
		if entry == gateway.BundlerRubyGemsFallbackTimeoutEnvKey+"=0" {
			t.Fatalf("did not expect bundler fallback timeout env without live rubygems route, got %v", env)
		}
		if entry == gateway.BundlerRubyGemsMirrorEnvKey+"=http://"+gateway.GuestGatewayHostname+":8170/rubygems/" {
			t.Fatalf("did not expect rubygems mirror env without live rubygems route, got %v", env)
		}
	}
}

func TestGatewayEnvVarsGeneratesGitRewriteAndHeader(t *testing.T) {
	t.Parallel()
	p := &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow: []policy.AllowRule{
			{Host: "github.com", Ports: []int{443}},
			{Host: "gitlab.com", Ports: []int{443}},
		},
	}
	env := gatewayEnvVars(p, 8170, "scope-token", gateway.ProxyRoutes{})
	if len(env) != 7 {
		t.Fatalf("expected 7 env vars (count + 3 key/value entries), got %d: %v", len(env), env)
	}
	if env[0] != "GIT_CONFIG_COUNT=3" {
		t.Fatalf("expected GIT_CONFIG_COUNT=3, got %s", env[0])
	}
	if !strings.Contains(env[1], "url.http://"+gateway.GuestGatewayHostname+":8170/git/github.com/.insteadOf") {
		t.Fatalf("expected github rewrite key, got %s", env[1])
	}
	if env[2] != "GIT_CONFIG_VALUE_0=https://github.com/" {
		t.Fatalf("expected github rewrite value, got %s", env[2])
	}
	if !strings.Contains(env[3], "url.http://"+gateway.GuestGatewayHostname+":8170/git/gitlab.com/.insteadOf") {
		t.Fatalf("expected gitlab rewrite key, got %s", env[3])
	}
	if env[4] != "GIT_CONFIG_VALUE_1=https://gitlab.com/" {
		t.Fatalf("expected gitlab rewrite value, got %s", env[4])
	}
	if !strings.Contains(env[5], "http.http://"+gateway.GuestGatewayHostname+":8170/.extraHeader") {
		t.Fatalf("expected extraHeader key, got %s", env[5])
	}
	wantHeader := "GIT_CONFIG_VALUE_2=" + gateway.ScopeTokenHeader + ": scope-token"
	if env[6] != wantHeader {
		t.Fatalf("expected %q, got %q", wantHeader, env[6])
	}
}

func TestGatewayGitProxyEnvVarsUsesFileHandleGatewayWithoutHeader(t *testing.T) {
	t.Parallel()

	p := &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
	}
	env := gatewayGitProxyEnvVars(p, darwinVZNetworkModeFileHandle, 8170, gateway.ProxyRoutes{})
	if len(env) != 3 {
		t.Fatalf("expected 3 env vars (count + 1 key/value entry), got %d: %v", len(env), env)
	}
	if env[0] != "GIT_CONFIG_COUNT=1" {
		t.Fatalf("expected GIT_CONFIG_COUNT=1, got %s", env[0])
	}
	if !strings.Contains(env[1], "url.http://"+gateway.GuestGatewayHostname+":8170/git/github.com/.insteadOf") {
		t.Fatalf("expected github rewrite key, got %s", env[1])
	}
	if env[2] != "GIT_CONFIG_VALUE_0=https://github.com/" {
		t.Fatalf("expected github rewrite value, got %s", env[2])
	}
}

func TestGatewayGitProxyEnvVarsDefaultsEmptyModeToFileHandle(t *testing.T) {
	t.Parallel()

	p := &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow:          []policy.AllowRule{{Host: "rubygems.org", Ports: []int{443}}},
	}
	env := gatewayGitProxyEnvVars(p, "", 8170, gateway.ProxyRoutes{RubyGems: true})
	wantAppConfig := gateway.BundlerAppConfigEnvKey + "=" + gateway.BundlerAppConfigPath
	wantMirror := gateway.BundlerRubyGemsMirrorEnvKey + "=http://" + gateway.GuestGatewayHostname + ":8170/rubygems/"
	foundAppConfig := false
	foundMirror := false
	for _, entry := range env {
		if entry == wantAppConfig {
			foundAppConfig = true
		}
		if entry == wantMirror {
			foundMirror = true
			break
		}
	}
	if !foundAppConfig {
		t.Fatalf("expected Bundler app config env %q in %v", wantAppConfig, env)
	}
	if !foundMirror {
		t.Fatalf("expected rubyGems mirror env %q in %v", wantMirror, env)
	}
}

func TestGatewayGitProxyEnvVarsSkipsNonFileHandleModes(t *testing.T) {
	t.Parallel()

	p := &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
	}

	for _, networkMode := range []string{"nat", "vmnet-shared", "other"} {
		networkMode := networkMode
		t.Run(networkMode, func(t *testing.T) {
			t.Parallel()
			if env := gatewayGitProxyEnvVars(p, networkMode, 8170, gateway.ProxyRoutes{RubyGems: true}); env != nil {
				t.Fatalf("expected no git proxy env for non-filehandle mode %q, got %v", networkMode, env)
			}
		})
	}
}

func TestGatewayEnvVarsAddsGoProxyAndMiseMirror(t *testing.T) {
	t.Parallel()

	p := &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow: []policy.AllowRule{
			{Host: "proxy.golang.org", Ports: []int{443}},
			{Host: "dl.google.com", Ports: []int{443}},
		},
	}
	env := gatewayEnvVars(p, 8170, "token", gateway.ProxyRoutes{GoProxy: true, Fetch: true})
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
