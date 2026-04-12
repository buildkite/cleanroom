package darwinvz

import (
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/policy"
)

func TestGatewayEnvVarsNilPolicy(t *testing.T) {
	t.Parallel()
	env := gatewayEnvVars(nil, 8170, "token")
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
	env := gatewayEnvVars(p, 8170, "token")
	if env != nil {
		t.Fatalf("expected nil env without port 443 hosts, got %v", env)
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
	env := gatewayEnvVars(p, 8170, "scope-token")
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
	env := gatewayGitProxyEnvVars(p, darwinVZNetworkModeFileHandle, 8170)
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
			if env := gatewayGitProxyEnvVars(p, networkMode, 8170); env != nil {
				t.Fatalf("expected no git proxy env for non-filehandle mode %q, got %v", networkMode, env)
			}
		})
	}
}
