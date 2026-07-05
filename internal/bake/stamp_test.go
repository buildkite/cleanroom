package bake

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

func TestStampRecordsCleanroomFacts(t *testing.T) {
	cwd := t.TempDir()
	compiled := &policy.CompiledPolicy{
		ImageRef:    testImageRef,
		ImageDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Hash:        "policy-hash",
	}

	got, err := Stamp(
		cwd,
		filepath.Join(cwd, "cleanroom.yaml"),
		compiled,
		"v0.1.0",
		[]NetworkRule{{Host: "github.com", Ports: []uint16{443, 8443}}},
	)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}

	wantPairs := map[string]string{
		"dev.buildkite.cleanroom.provenance.version": "1",
		"dev.buildkite.cleanroom.version":            "v0.1.0",
		"dev.buildkite.cleanroom.policy.hash":        "policy-hash",
		"dev.buildkite.cleanroom.policy.source":      filepath.Join(cwd, "cleanroom.yaml"),
		"dev.buildkite.cleanroom.image.ref":          compiled.ImageRef,
		"dev.buildkite.cleanroom.image.digest":       compiled.ImageDigest,
		"dev.buildkite.cleanroom.workspace.dir":      cwd,
	}
	for key, want := range wantPairs {
		if got[key] != want {
			t.Fatalf("annotation %s = %q, want %q", key, got[key], want)
		}
	}

	type networkRuleAnnotation struct {
		Host  string   `json:"host"`
		Ports []uint16 `json:"ports"`
	}
	var rules []networkRuleAnnotation
	if err := json.Unmarshal([]byte(got["dev.buildkite.cleanroom.network.rules"]), &rules); err != nil {
		t.Fatalf("decode network rules annotation: %v", err)
	}
	wantRules := []networkRuleAnnotation{{Host: "github.com", Ports: []uint16{443, 8443}}}
	if !reflect.DeepEqual(rules, wantRules) {
		t.Fatalf("network rules annotation = %#v, want %#v", rules, wantRules)
	}
}

func TestStampOmitsNetworkRulesWhenEmpty(t *testing.T) {
	got, err := Stamp(t.TempDir(), "", &policy.CompiledPolicy{}, "", nil)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if _, ok := got["dev.buildkite.cleanroom.network.rules"]; ok {
		t.Fatal("network rules annotation should be omitted when there are no rules")
	}
}

func TestStampRecordsGitFactsWhenAvailable(t *testing.T) {
	cwd := t.TempDir()
	runGit(t, cwd, "init")
	runGit(t, cwd, "config", "user.email", "cleanroom-test@example.com")
	runGit(t, cwd, "config", "user.name", "Cleanroom Test")
	if err := os.WriteFile(filepath.Join(cwd, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGit(t, cwd, "add", "README.md")
	runGit(t, cwd, "-c", "commit.gpgsign=false", "commit", "-m", "initial")
	runGit(t, cwd, "remote", "add", "origin", "https://example.com/acme/repo.git")
	if err := os.WriteFile(filepath.Join(cwd, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty.txt: %v", err)
	}

	got, err := Stamp(cwd, "", &policy.CompiledPolicy{}, "", nil)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}

	commit := runGit(t, cwd, "rev-parse", "HEAD")
	wantPairs := map[string]string{
		"dev.buildkite.cleanroom.workspace.git.commit": commit,
		"dev.buildkite.cleanroom.workspace.git.remote": "https://example.com/acme/repo.git",
		"dev.buildkite.cleanroom.workspace.git.dirty":  "true",
	}
	for key, want := range wantPairs {
		if got[key] != want {
			t.Fatalf("annotation %s = %q, want %q", key, got[key], want)
		}
	}
}

func TestCollectGitFactsExcludingIgnoresOutArtifact(t *testing.T) {
	cwd := t.TempDir()
	runGit(t, cwd, "init")
	runGit(t, cwd, "config", "user.email", "cleanroom-test@example.com")
	runGit(t, cwd, "config", "user.name", "Cleanroom Test")
	if err := os.WriteFile(filepath.Join(cwd, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGit(t, cwd, "add", "README.md")
	runGit(t, cwd, "-c", "commit.gpgsign=false", "commit", "-m", "initial")

	// Simulate a prior `bake --out repo.spore` writing inside the repo.
	if err := os.WriteFile(filepath.Join(cwd, "repo.spore"), []byte("artifact"), 0o644); err != nil {
		t.Fatalf("write repo.spore: %v", err)
	}

	if facts := CollectGitFacts(cwd); !facts.Dirty {
		t.Fatal("CollectGitFacts should report dirty when artifact is present")
	}
	if facts := CollectGitFactsExcluding(cwd, []string{"repo.spore"}); facts.Dirty {
		t.Fatal("CollectGitFactsExcluding should not treat the bake artifact as uncommitted source")
	}

	// A genuinely dirty file still counts even with the exclusion.
	if err := os.WriteFile(filepath.Join(cwd, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty.txt: %v", err)
	}
	if facts := CollectGitFactsExcluding(cwd, []string{"repo.spore"}); !facts.Dirty {
		t.Fatal("CollectGitFactsExcluding should still report other uncommitted changes")
	}
}

func TestAnnotationArgsAreSortedAndPaired(t *testing.T) {
	got := AnnotationArgs(map[string]string{
		"b.key": "2",
		"a.key": "1",
	})
	want := []string{"--annotation", "a.key=1", "--annotation", "b.key=2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("annotation args = %v, want %v", got, want)
	}
}

func TestQuoteArgsShellSafety(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"--image", "ghcr.io/a@sha256:abc"}, "--image ghcr.io/a@sha256:abc"},
		{[]string{"--annotation", "k=v with space"}, `--annotation 'k=v with space'`},
		{[]string{"a'b"}, `'a'\''b'`},
		{[]string{""}, "''"},
		{[]string{"$HOME", "a;b", "a|b"}, `'$HOME' 'a;b' 'a|b'`},
	}
	for _, tc := range tests {
		if got := QuoteArgs(tc.args); got != tc.want {
			t.Fatalf("QuoteArgs(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestStampRecordsMediationAndGatewayServices(t *testing.T) {
	compiled := &policy.CompiledPolicy{
		ImageRef:  testImageRef,
		Hash:      "policy-hash",
		Mediation: []string{"anthropic-inference", "github-token"},
	}
	got, err := Stamp(t.TempDir(), "", compiled, "v0.1.0", nil)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	var services []string
	if err := json.Unmarshal([]byte(got["dev.buildkite.cleanroom.mediation.services"]), &services); err != nil {
		t.Fatalf("decode mediation annotation: %v", err)
	}
	if !reflect.DeepEqual(services, compiled.Mediation) {
		t.Fatalf("mediation services = %v", services)
	}
	var gateways []GatewayService
	if err := json.Unmarshal([]byte(got["dev.buildkite.cleanroom.gateway.services"]), &gateways); err != nil {
		t.Fatalf("decode gateway annotation: %v", err)
	}
	want := []GatewayService{{Name: "cleanroom-gateway", GuestHost: "cleanroom-gateway.spore.internal", GuestPort: 8170}}
	if !reflect.DeepEqual(gateways, want) {
		t.Fatalf("gateway services = %#v", gateways)
	}
}
