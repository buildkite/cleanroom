package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/sporevm"
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

func TestValidateVMPolicyAllowsExactNetworkAllow(t *testing.T) {
	compiled := &policy.CompiledPolicy{
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
		Allow: []policy.AllowRule{
			{Host: "github.com", Ports: []int{443}},
		},
	}

	if err := validateVMPolicy(compiled); err != nil {
		t.Fatalf("validate network allow policy: %v", err)
	}
}

func TestValidateVMPolicyRejectsUntranslatedFeatures(t *testing.T) {
	tests := []struct {
		name     string
		policy   *policy.CompiledPolicy
		contains string
	}{
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

func TestValidateVMPolicyRejectsMalformedNetworkAllow(t *testing.T) {
	tests := []struct {
		name     string
		rule     policy.AllowRule
		contains string
	}{
		{name: "empty host", rule: policy.AllowRule{Ports: []int{443}}, contains: "include a host"},
		{name: "missing ports", rule: policy.AllowRule{Host: "github.com"}, contains: "at least one port"},
		{name: "bad port", rule: policy.AllowRule{Host: "github.com", Ports: []int{0}}, contains: "port 0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			compiled := &policy.CompiledPolicy{
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				NetworkDefault: "deny",
				Allow:          []policy.AllowRule{tc.rule},
			}

			err := validateVMPolicy(compiled)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected error containing %q, got %v", tc.contains, err)
			}
		})
	}
}

func TestVMNetworkRulesTranslatesExactAllows(t *testing.T) {
	compiled := &policy.CompiledPolicy{
		Allow: []policy.AllowRule{
			{Host: "github.com", Ports: []int{443, 8443}},
			{Host: "api.github.com", Ports: []int{443}},
		},
	}

	got, err := vmNetworkRules(compiled)
	if err != nil {
		t.Fatalf("network rules: %v", err)
	}
	want := []sporevm.NetworkRule{
		{Host: "github.com", Ports: []uint16{443, 8443}},
		{Host: "api.github.com", Ports: []uint16{443}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected network rules: got %#v want %#v", got, want)
	}
}
