package cli

import (
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

func TestValidateVMPolicyAllowsMinimalPolicy(t *testing.T) {
	compiled := &policy.CompiledPolicy{
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
	}

	if err := validateVMPolicy(compiled); err != nil {
		t.Fatalf("validate minimal policy: %v", err)
	}
}

func TestValidateVMPolicyRejectsUntranslatedFeatures(t *testing.T) {
	tests := []struct {
		name     string
		policy   *policy.CompiledPolicy
		contains string
	}{
		{
			name: "network allow",
			policy: &policy.CompiledPolicy{
				ImageRef: "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				Allow:    []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
			},
			contains: "network allow",
		},
		{
			name: "docker service",
			policy: &policy.CompiledPolicy{
				ImageRef: "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				Docker:   policy.DockerService{Required: true},
			},
			contains: "docker service",
		},
		{
			name: "stage scoped network",
			policy: &policy.CompiledPolicy{
				ImageRef: "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				NetworkStages: &policy.NetworkStagePolicies{
					Execution: &policy.NetworkPolicy{},
				},
			},
			contains: "stage-scoped network",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVMPolicy(tc.policy)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected error containing %q, got %v", tc.contains, err)
			}
		})
	}
}
