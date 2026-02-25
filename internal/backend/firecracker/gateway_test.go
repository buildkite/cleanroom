package firecracker

import (
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

func TestGatewayEnvVarsEmptyWhenNoPolicyHosts(t *testing.T) {
	t.Parallel()

	instance := &sandboxInstance{
		HostIP: "10.1.1.1",
		Policy: &policy.CompiledPolicy{
			Version:        1,
			NetworkDefault: "deny",
			Allow:          []policy.AllowRule{{Host: "example.com", Ports: []int{80}}},
		},
	}
	env := gatewayEnvVars(instance, 8170)
	if len(env) != 0 {
		t.Fatalf("expected no env vars for host without port 443, got %v", env)
	}
}

func TestGatewayEnvVarsGeneratesGitConfig(t *testing.T) {
	t.Parallel()

	instance := &sandboxInstance{
		HostIP: "10.1.1.1",
		Policy: &policy.CompiledPolicy{
			Version:        1,
			NetworkDefault: "deny",
			Allow: []policy.AllowRule{
				{Host: "github.com", Ports: []int{443}},
				{Host: "gitlab.com", Ports: []int{443}},
			},
		},
	}

	env := gatewayEnvVars(instance, 8170)
	if len(env) != 5 {
		t.Fatalf("expected 5 env vars (1 count + 2*2 key/value), got %d: %v", len(env), env)
	}

	if env[0] != "GIT_CONFIG_COUNT=2" {
		t.Fatalf("expected GIT_CONFIG_COUNT=2, got %s", env[0])
	}

	if !strings.Contains(env[1], "url.http://10.1.1.1:8170/git/github.com/.insteadOf") {
		t.Fatalf("expected github.com insteadOf key, got %s", env[1])
	}
	if env[2] != "GIT_CONFIG_VALUE_0=https://github.com/" {
		t.Fatalf("expected github.com value, got %s", env[2])
	}
	if !strings.Contains(env[3], "url.http://10.1.1.1:8170/git/gitlab.com/.insteadOf") {
		t.Fatalf("expected gitlab.com insteadOf key, got %s", env[3])
	}
}

func TestGatewayEnvVarsNilPolicy(t *testing.T) {
	t.Parallel()

	instance := &sandboxInstance{HostIP: "10.1.1.1"}
	env := gatewayEnvVars(instance, 8170)
	if env != nil {
		t.Fatalf("expected nil for nil policy, got %v", env)
	}
}
