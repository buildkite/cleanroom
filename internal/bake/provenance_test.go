package bake

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

// loadInspectFixture pins the annotation merge-through-snapshot behavior: the
// fixture is real `spore --json inspect` output for a cleanroom-baked spore.
// If upstream stops merging create-time annotations into captured manifests,
// this fixture (regenerated) will fail parsing and flag the contract break.
func loadInspectFixture(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("testdata/inspect-baked.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var inspect struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(raw, &inspect); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return inspect.Annotations
}

func TestParseProvenanceFromRecordedInspectOutput(t *testing.T) {
	prov, err := ParseProvenance(loadInspectFixture(t))
	if err != nil {
		t.Fatalf("parse provenance: %v", err)
	}
	if prov.Version != ProvenanceVersion {
		t.Fatalf("version = %q", prov.Version)
	}
	if prov.BakeKey == "" || prov.PolicyHash == "" || prov.ImageRef == "" {
		t.Fatalf("missing create facts: %+v", prov)
	}
	if prov.GitCommit == "" || prov.GitRemote != "https://example.com/acme/fixture.git" {
		t.Fatalf("missing git facts: %+v", prov)
	}
	if prov.GitDirty {
		t.Fatal("fixture was baked from a clean workspace")
	}
	wantRules := []NetworkRule{{Host: "dl-cdn.alpinelinux.org", Ports: []uint16{443}}}
	if !reflect.DeepEqual(prov.NetworkRules, wantRules) {
		t.Fatalf("network rules = %#v", prov.NetworkRules)
	}
	if len(prov.GatewayServices) != 0 {
		t.Fatalf("fixture has no gateway services: %#v", prov.GatewayServices)
	}
}

func TestParseProvenanceFailsClosed(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		contains    string
	}{
		{
			name:        "foreign spore",
			annotations: map[string]string{},
			contains:    "missing cleanroom provenance",
		},
		{
			name: "unsupported version",
			annotations: map[string]string{
				AnnotationPrefix + "provenance.version": "2",
			},
			contains: "unsupported cleanroom provenance version",
		},
		{
			name: "missing create facts",
			annotations: map[string]string{
				AnnotationPrefix + "provenance.version": "1",
			},
			contains: "missing cleanroom create provenance",
		},
		{
			name: "malformed network rules",
			annotations: map[string]string{
				AnnotationPrefix + "provenance.version": "1",
				AnnotationPrefix + "policy.hash":        "h",
				AnnotationPrefix + "network.rules":      "{not json",
			},
			contains: "decode cleanroom network rule provenance",
		},
		{
			name: "network rule without ports",
			annotations: map[string]string{
				AnnotationPrefix + "provenance.version": "1",
				AnnotationPrefix + "policy.hash":        "h",
				AnnotationPrefix + "network.rules":      `[{"host":"github.com","ports":[]}]`,
			},
			contains: "missing ports",
		},
		{
			name: "gateway service without name",
			annotations: map[string]string{
				AnnotationPrefix + "provenance.version": "1",
				AnnotationPrefix + "policy.hash":        "h",
				AnnotationPrefix + "gateway.services":   `[{"guest_host":"gateway.cleanroom.internal","guest_port":8170}]`,
			},
			contains: "missing name",
		},
		{
			name: "control characters in fact",
			annotations: map[string]string{
				AnnotationPrefix + "provenance.version": "1",
				AnnotationPrefix + "policy.hash":        "h",
				AnnotationPrefix + "image.ref":          "ghcr.io/x\x1b[31mforged",
			},
			contains: "control characters",
		},
		{
			name: "gateway service name with shell metacharacters",
			annotations: map[string]string{
				AnnotationPrefix + "provenance.version": "1",
				AnnotationPrefix + "policy.hash":        "h",
				AnnotationPrefix + "gateway.services":   `[{"name":"g=unix:x '; rm -rf ~","guest_host":"gateway.internal","guest_port":8170}]`,
			},
			contains: "invalid name",
		},
		{
			name: "network host with spaces",
			annotations: map[string]string{
				AnnotationPrefix + "provenance.version": "1",
				AnnotationPrefix + "policy.hash":        "h",
				AnnotationPrefix + "network.rules":      `[{"host":"evil host","ports":[443]}]`,
			},
			contains: "invalid host",
		},
		{
			name: "invalid dirty value",
			annotations: map[string]string{
				AnnotationPrefix + "provenance.version":  "1",
				AnnotationPrefix + "policy.hash":         "h",
				AnnotationPrefix + "workspace.git.dirty": "1",
			},
			contains: "invalid value",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseProvenance(tc.annotations)
			if err == nil {
				t.Fatal("expected parse error")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected error containing %q, got %v", tc.contains, err)
			}
		})
	}
}

func TestParseProvenanceGatewayServices(t *testing.T) {
	prov, err := ParseProvenance(map[string]string{
		AnnotationPrefix + "provenance.version": "1",
		AnnotationPrefix + "policy.hash":        "h",
		AnnotationPrefix + "gateway.services":   `[{"name":"cleanroom-gateway","guest_host":"gateway.cleanroom.internal","guest_port":8170}]`,
	})
	if err != nil {
		t.Fatalf("parse provenance: %v", err)
	}
	want := []GatewayService{{Name: "cleanroom-gateway", GuestHost: "gateway.cleanroom.internal", GuestPort: 8170}}
	if !reflect.DeepEqual(prov.GatewayServices, want) {
		t.Fatalf("gateway services = %#v", prov.GatewayServices)
	}
	invocation := prov.RunFromInvocation("./captured.spore")
	wantInvocation := "spore run --from ./captured.spore --bind-service cleanroom-gateway=unix:/path/to/cleanroom-gateway.sock 'COMMAND'"
	if invocation != wantInvocation {
		t.Fatalf("invocation = %q, want %q", invocation, wantInvocation)
	}
}

func TestRunFromInvocationWithoutServices(t *testing.T) {
	prov := Provenance{Version: "1"}
	if got, want := prov.RunFromInvocation("/tmp/x.spore"), "spore run --from /tmp/x.spore 'COMMAND'"; got != want {
		t.Fatalf("invocation = %q, want %q", got, want)
	}
}

func TestAuditKey(t *testing.T) {
	compiled := &policy.CompiledPolicy{ImageRef: testImageRef, Hash: "policy-hash"}
	facts := GitFacts{Commit: "abc", HasGit: true}
	key := Key(compiled, facts)

	if err := AuditKey(Provenance{BakeKey: key}, compiled, facts); err != nil {
		t.Fatalf("matching audit: %v", err)
	}
	err := AuditKey(Provenance{BakeKey: "stale"}, compiled, facts)
	if err == nil || !strings.Contains(err.Error(), "bake key mismatch") {
		t.Fatalf("expected mismatch, got %v", err)
	}
	err = AuditKey(Provenance{}, compiled, facts)
	if err == nil || !strings.Contains(err.Error(), "records no bake key") {
		t.Fatalf("expected missing key error, got %v", err)
	}
	dirty := GitFacts{Commit: "abc", Dirty: true, HasGit: true}
	err = AuditKey(Provenance{BakeKey: Key(compiled, dirty)}, compiled, dirty)
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("expected dirty rejection, got %v", err)
	}
}
