package bake

import (
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

const testImageRef = "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestCompileAllowsMinimalPolicy(t *testing.T) {
	inputs, err := Compile(&policy.CompiledPolicy{
		ImageRef:       testImageRef,
		NetworkDefault: "deny",
	})
	if err != nil {
		t.Fatalf("compile minimal policy: %v", err)
	}
	if inputs.ImageRef != testImageRef {
		t.Fatalf("image ref = %q", inputs.ImageRef)
	}
}

func TestCompileTranslatesResourcesAndNetwork(t *testing.T) {
	inputs, err := Compile(&policy.CompiledPolicy{
		ImageRef:       testImageRef,
		NetworkDefault: "deny",
		Resources: &policy.Resources{
			VCPUs:       4,
			MemoryBytes: 2 << 30,
		},
		Allow: []policy.AllowRule{
			{Host: "github.com", Ports: []int{443, 8443}},
			{Host: "api.github.com", Ports: []int{443}},
		},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	want := CreateInputs{
		ImageRef:    testImageRef,
		MemoryBytes: 2 << 30,
		VCPUs:       4,
		NetworkRules: []NetworkRule{
			{Host: "github.com", Ports: []uint16{443, 8443}},
			{Host: "api.github.com", Ports: []uint16{443}},
		},
	}
	if !reflect.DeepEqual(inputs, want) {
		t.Fatalf("inputs = %#v, want %#v", inputs, want)
	}
}

func TestCompileRejectsUntranslatedFeatures(t *testing.T) {
	tests := []struct {
		name     string
		policy   *policy.CompiledPolicy
		contains string
	}{
		{
			name:     "missing image",
			policy:   &policy.CompiledPolicy{},
			contains: "sandbox.image.ref",
		},
		{
			name: "disk resources",
			policy: &policy.CompiledPolicy{
				ImageRef:  testImageRef,
				Resources: &policy.Resources{DiskBytes: 16 << 30},
			},
			contains: "sandbox.resources.disk",
		},

		{
			name: "vcpus overflow",
			policy: &policy.CompiledPolicy{
				ImageRef:  testImageRef,
				Resources: &policy.Resources{VCPUs: 1 << 33},
			},
			contains: "does not support vcpus",
		},
		{
			name: "docker service",
			policy: &policy.CompiledPolicy{
				ImageRef: testImageRef,
				Docker:   policy.DockerService{Required: true},
			},
			contains: "docker service",
		},
		{
			name: "stage scoped network",
			policy: &policy.CompiledPolicy{
				ImageRef: testImageRef,
				NetworkStages: &policy.NetworkStagePolicies{
					Execution: &policy.NetworkPolicy{},
				},
			},
			contains: "stage-scoped network",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.policy)
			if err == nil {
				t.Fatal("expected compile error")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected error containing %q, got %v", tc.contains, err)
			}
		})
	}
}

func TestCompileRejectsMalformedNetworkAllow(t *testing.T) {
	tests := []struct {
		name     string
		rule     policy.AllowRule
		contains string
	}{
		{name: "empty host", rule: policy.AllowRule{Ports: []int{443}}, contains: "include a host"},
		{name: "missing ports", rule: policy.AllowRule{Host: "github.com"}, contains: "at least one port"},
		{name: "bad port", rule: policy.AllowRule{Host: "github.com", Ports: []int{0}}, contains: "port 0"},
		// Policy accepts IPv6 literal hosts, but provenance parsing rejects
		// ':' so a baked spore would fail its own verify; compile must fail
		// closed until SporeVM has a supported encoding.
		{name: "ipv6 literal host", rule: policy.AllowRule{Host: "2606:4700:4700::1111", Ports: []int{443}}, contains: "does not yet support network allow host"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(&policy.CompiledPolicy{
				ImageRef:       testImageRef,
				NetworkDefault: "deny",
				Allow:          []policy.AllowRule{tc.rule},
			})
			if err == nil {
				t.Fatal("expected compile error")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected error containing %q, got %v", tc.contains, err)
			}
		})
	}
}

func TestCreateInputsArgs(t *testing.T) {
	inputs := CreateInputs{
		ImageRef:    testImageRef,
		MemoryBytes: 2 << 30,
		VCPUs:       4,
		NetworkRules: []NetworkRule{
			{Host: "github.com", Ports: []uint16{443, 8443}},
		},
	}
	want := []string{
		"--image", testImageRef,
		"--memory", "2gb",
		"--vcpus", "4",
		"--net",
		"--allow-host-port", "github.com:443",
		"--allow-host-port", "github.com:8443",
	}
	if got := inputs.Args(); !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestCreateInputsArgsOmitsUnsetOptions(t *testing.T) {
	inputs := CreateInputs{ImageRef: testImageRef}
	want := []string{"--image", testImageRef}
	if got := inputs.Args(); !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestCompilePageAlignsDecimalPolicyMemory(t *testing.T) {
	// Policy sizes are decimal (1gb = 10^9), spore requires page alignment.
	inputs, err := Compile(&policy.CompiledPolicy{
		ImageRef:  testImageRef,
		Resources: &policy.Resources{MemoryBytes: 1_000_000_000},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if inputs.MemoryBytes%16384 != 0 {
		t.Fatalf("memory %d is not 16KiB-page-aligned", inputs.MemoryBytes)
	}
	if got, want := inputs.MemoryBytes, uint64(1_000_013_824); got != want {
		t.Fatalf("memory = %d, want %d (rounded up to next page)", got, want)
	}
}

func TestFormatMemoryUsesSmallestExactUnit(t *testing.T) {
	tests := []struct {
		bytes uint64
		want  string
	}{
		{2 << 30, "2gb"},
		{512 << 20, "512mb"},
		{1536 << 20, "1536mb"},
		{8 << 10, "8kb"},
		{4096, "4kb"},
	}
	for _, tc := range tests {
		if got := formatMemory(tc.bytes); got != tc.want {
			t.Fatalf("formatMemory(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}
