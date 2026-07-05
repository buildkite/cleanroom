package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/buildkite/cleanroom/internal/bake"
	"github.com/buildkite/cleanroom/internal/policy"
)

type stubLoader struct {
	compiled *policy.CompiledPolicy
}

func (l stubLoader) LoadAndCompile(string) (*policy.CompiledPolicy, string, error) {
	return l.compiled, "cleanroom.yaml", nil
}

func runGitCLI(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestVerifyDirAuditsInRepoArtifact reproduces the documented flow
// `cleanroom bake . --out repo.spore` followed by `verify repo.spore --dir .`:
// the artifact is an untracked child of the repository, and the audit must
// exclude it from dirty detection exactly as bake did when computing the key.
func TestVerifyDirAuditsInRepoArtifact(t *testing.T) {
	repo := t.TempDir()
	runGitCLI(t, repo, "init")
	runGitCLI(t, repo, "config", "user.email", "cleanroom-test@example.com")
	runGitCLI(t, repo, "config", "user.name", "Cleanroom Test")
	if err := os.WriteFile(filepath.Join(repo, "cleanroom.yaml"), []byte("policy\n"), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	runGitCLI(t, repo, "add", "cleanroom.yaml")
	runGitCLI(t, repo, "-c", "commit.gpgsign=false", "commit", "-m", "initial")

	compiled := &policy.CompiledPolicy{
		ImageRef:    "ghcr.io/x@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Hash:        "policy-hash",
	}

	// The spore artifact bake wrote inside the repo: untracked, and its key
	// was computed with the artifact excluded from dirty detection.
	sporeDir := filepath.Join(repo, "repo.spore")
	if err := os.MkdirAll(sporeDir, 0o755); err != nil {
		t.Fatalf("mkdir spore dir: %v", err)
	}
	// Git ignores empty directories; a real spore contains files.
	if err := os.WriteFile(filepath.Join(sporeDir, "manifest.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write spore manifest: %v", err)
	}
	facts := bake.CollectGitFactsExcluding(repo, bake.ArtifactExclusions(repo, sporeDir))
	if facts.Dirty {
		t.Fatal("setup: facts should be clean with the artifact excluded")
	}
	annotations := map[string]string{
		bake.AnnotationPrefix + "provenance.version":   bake.ProvenanceVersion,
		bake.AnnotationPrefix + "bake.key":             bake.Key(compiled, facts),
		bake.AnnotationPrefix + "policy.hash":          compiled.Hash,
		bake.AnnotationPrefix + "image.ref":            compiled.ImageRef,
		bake.AnnotationPrefix + "image.digest":         compiled.ImageDigest,
		bake.AnnotationPrefix + "workspace.dir":        repo,
		bake.AnnotationPrefix + "workspace.git.commit": facts.Commit,
		bake.AnnotationPrefix + "workspace.git.dirty":  "false",
	}
	inspectJSON, err := json.Marshal(map[string]any{"annotations": annotations})
	if err != nil {
		t.Fatalf("encode inspect payload: %v", err)
	}

	// Fake spore executable: answers --json inspect with the annotations.
	binDir := t.TempDir()
	fakeSpore := filepath.Join(binDir, "spore")
	payloadPath := filepath.Join(binDir, "inspect.json")
	if err := os.WriteFile(payloadPath, inspectJSON, 0o644); err != nil {
		t.Fatalf("write inspect payload: %v", err)
	}
	script := "#!/bin/sh\ncat " + payloadPath + "\n"
	if err := os.WriteFile(fakeSpore, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake spore: %v", err)
	}

	stdout, readStdout := makeStdoutCapture(t)
	cmd := &VerifyCommand{SporeDir: sporeDir, Dir: repo, Spore: fakeSpore}
	err = cmd.Run(&runtimeContext{
		CWD:    repo,
		Stdout: stdout,
		Loader: stubLoader{compiled: compiled},
	})
	if err != nil {
		t.Fatalf("verify --dir with in-repo artifact: %v", err)
	}
	assertContainsAll(t, readStdout(),
		"bake key matches the repository's current policy and commit",
		"verified",
	)
}
