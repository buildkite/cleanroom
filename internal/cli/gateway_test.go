package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/bake"
	"github.com/buildkite/cleanroom/internal/policy"
)

// TestGatewayServeRefusesForgedSpore proves annotations are not a trust
// root: a foreign spore that forges the remote, policy hash, and mediation
// requests of a granted lineage must be refused because its bake key cannot
// match the repository's current policy and commit.
func TestGatewayServeRefusesForgedSpore(t *testing.T) {
	repo := t.TempDir()
	runGitCLI(t, repo, "init")
	runGitCLI(t, repo, "config", "user.email", "cleanroom-test@example.com")
	runGitCLI(t, repo, "config", "user.name", "Cleanroom Test")
	if err := os.WriteFile(filepath.Join(repo, "cleanroom.yaml"), []byte("policy\n"), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	runGitCLI(t, repo, "add", "cleanroom.yaml")
	runGitCLI(t, repo, "-c", "commit.gpgsign=false", "commit", "-m", "initial")
	runGitCLI(t, repo, "remote", "add", "origin", "https://github.com/example-org/repo.git")

	compiled := &policy.CompiledPolicy{
		ImageRef:    "ghcr.io/x@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Hash:        "policy-hash",
		Mediation:   []string{"github-token"},
	}

	// Operator grants github-token to the repo's lineage.
	grantsPath := filepath.Join(t.TempDir(), "gateway.yaml")
	grants := `services:
  github-token:
    upstream: https://api.github.com
grants:
  - match: { remote: "https://github.com/example-org/*" }
    services: [github-token]
`
	if err := os.WriteFile(grantsPath, []byte(grants), 0o644); err != nil {
		t.Fatalf("write grants: %v", err)
	}

	// A foreign spore forging the granted lineage's facts. Its bake key is
	// forged too, but the forger cannot know the repository's real key
	// inputs, so any forged key fails the audit.
	forged := map[string]string{
		bake.AnnotationPrefix + "provenance.version":  bake.ProvenanceVersion,
		bake.AnnotationPrefix + "bake.key":            "forged-key",
		bake.AnnotationPrefix + "policy.hash":         compiled.Hash,
		bake.AnnotationPrefix + "image.ref":           compiled.ImageRef,
		bake.AnnotationPrefix + "image.digest":        compiled.ImageDigest,
		bake.AnnotationPrefix + "workspace.dir":       repo,
		bake.AnnotationPrefix + "workspace.git.dirty": "false",
		bake.AnnotationPrefix + "mediation.services":  `["github-token"]`,
	}
	inspectJSON, err := json.Marshal(map[string]any{"annotations": forged})
	if err != nil {
		t.Fatalf("encode inspect payload: %v", err)
	}
	binDir := t.TempDir()
	fakeSpore := filepath.Join(binDir, "spore")
	payloadPath := filepath.Join(binDir, "inspect.json")
	if err := os.WriteFile(payloadPath, inspectJSON, 0o644); err != nil {
		t.Fatalf("write inspect payload: %v", err)
	}
	if err := os.WriteFile(fakeSpore, []byte("#!/bin/sh\ncat "+payloadPath+"\n"), 0o755); err != nil {
		t.Fatalf("write fake spore: %v", err)
	}

	cmd := &GatewayServeCommand{
		For:    filepath.Join(t.TempDir(), "foreign.spore"),
		Dir:    repo,
		Socket: filepath.Join(t.TempDir(), "gw.sock"),
		Grants: grantsPath,
		Spore:  fakeSpore,
	}
	err = cmd.Run(&runtimeContext{
		CWD:    repo,
		Loader: stubLoader{compiled: compiled},
	})
	if err == nil {
		t.Fatal("expected gateway serve to refuse the forged spore")
	}
	if !strings.Contains(err.Error(), "refusing to serve") || !strings.Contains(err.Error(), "bake key mismatch") {
		t.Fatalf("expected bake key audit refusal, got %v", err)
	}
}
