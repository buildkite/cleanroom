package controlservice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
)

func TestBuildNetworkFilterPolicySnapshotUnionsAllowRules(t *testing.T) {
	svc := &Service{
		sandboxes: map[string]*sandboxState{
			"sb-ready-a": {
				Status: cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY,
				Policy: &policy.CompiledPolicy{
					NetworkDefault: "deny",
					Allow: []policy.AllowRule{
						{Host: "api.github.com", Ports: []int{443}},
						{Host: "proxy.golang.org", Ports: []int{443, 80}},
					},
				},
			},
			"sb-ready-b": {
				Status: cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY,
				Policy: &policy.CompiledPolicy{
					NetworkDefault: "deny",
					Allow: []policy.AllowRule{
						{Host: "proxy.golang.org", Ports: []int{443, 8080}},
					},
				},
			},
			"sb-stopped": {
				Status: cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED,
				Policy: &policy.CompiledPolicy{
					NetworkDefault: "deny",
					Allow: []policy.AllowRule{
						{Host: "should.not.appear", Ports: []int{443}},
					},
				},
			},
			"sb-allow-default": {
				Status: cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY,
				Policy: &policy.CompiledPolicy{
					NetworkDefault: "allow",
					Allow: []policy.AllowRule{
						{Host: "ignored.example", Ports: []int{443}},
					},
				},
			},
		},
	}

	snapshot := svc.buildNetworkFilterPolicySnapshot("/tmp/cleanroom-darwin-vz")

	if got, want := snapshot.Version, networkFilterPolicySchema; got != want {
		t.Fatalf("snapshot version = %d, want %d", got, want)
	}
	if got, want := snapshot.DefaultAction, networkFilterActionDeny; got != want {
		t.Fatalf("default action = %q, want %q", got, want)
	}
	if got, want := snapshot.TargetProcessPath, "/tmp/cleanroom-darwin-vz"; got != want {
		t.Fatalf("target process path = %q, want %q", got, want)
	}

	wantAllow := []networkFilterPolicyAllowRule{
		{Host: "api.github.com", Ports: []int{443}},
		{Host: "proxy.golang.org", Ports: []int{80, 443, 8080}},
	}
	if !reflect.DeepEqual(snapshot.Allow, wantAllow) {
		t.Fatalf("allow rules mismatch:\n got=%v\nwant=%v", snapshot.Allow, wantAllow)
	}
}

func TestBuildNetworkFilterPolicySnapshotDefaultsToAllow(t *testing.T) {
	svc := &Service{
		sandboxes: map[string]*sandboxState{},
	}

	snapshot := svc.buildNetworkFilterPolicySnapshot("")

	if got, want := snapshot.DefaultAction, networkFilterActionAllow; got != want {
		t.Fatalf("default action = %q, want %q", got, want)
	}
	if len(snapshot.Allow) != 0 {
		t.Fatalf("expected no allow rules, got %v", snapshot.Allow)
	}
}

func TestWriteNetworkFilterPolicySnapshotWritesJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cleanroom", "network-filter-policy.json")
	in := networkFilterPolicySnapshot{
		Version:       networkFilterPolicySchema,
		UpdatedAt:     "2026-03-01T00:00:00Z",
		DefaultAction: networkFilterActionDeny,
		Allow: []networkFilterPolicyAllowRule{
			{Host: "example.com", Ports: []int{443}},
		},
	}

	if err := writeNetworkFilterPolicySnapshot(path, in); err != nil {
		t.Fatalf("writeNetworkFilterPolicySnapshot returned error: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot file: %v", err)
	}

	var out networkFilterPolicySnapshot
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal snapshot json: %v", err)
	}

	if !reflect.DeepEqual(out, in) {
		t.Fatalf("snapshot mismatch:\n got=%+v\nwant=%+v", out, in)
	}
}

func TestCreateSandboxSyncsNetworkFilterPolicySnapshot(t *testing.T) {
	policyPath := filepath.Join(t.TempDir(), "cleanroom", "network-filter-policy.json")
	t.Setenv(networkFilterPolicyPathEnv, policyPath)
	t.Setenv(networkFilterTargetProcessEnv, "/tmp/cleanroom-darwin-vz")

	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	req := &cleanroomv1.CreateSandboxRequest{
		Policy: &cleanroomv1.Policy{
			Version:        1,
			ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			NetworkDefault: "deny",
			Allow: []*cleanroomv1.PolicyAllowRule{
				{Host: "api.github.com", Ports: []int32{443}},
			},
		},
	}
	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	raw, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatalf("read policy snapshot: %v", err)
	}

	var snapshot networkFilterPolicySnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("unmarshal policy snapshot: %v", err)
	}
	if got, want := snapshot.DefaultAction, networkFilterActionDeny; got != want {
		t.Fatalf("default action = %q, want %q", got, want)
	}
	if got, want := snapshot.TargetProcessPath, "/tmp/cleanroom-darwin-vz"; got != want {
		t.Fatalf("target process path = %q, want %q", got, want)
	}
	if len(snapshot.Allow) != 1 {
		t.Fatalf("expected 1 allow rule, got %d (%v)", len(snapshot.Allow), snapshot.Allow)
	}
	if got, want := snapshot.Allow[0].Host, "api.github.com"; got != want {
		t.Fatalf("allow host = %q, want %q", got, want)
	}
	if got, want := snapshot.Allow[0].Ports, []int{443}; !reflect.DeepEqual(got, want) {
		t.Fatalf("allow ports = %v, want %v", got, want)
	}
}
