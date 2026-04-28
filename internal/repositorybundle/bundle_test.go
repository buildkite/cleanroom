package repositorybundle

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

func TestBuildFromRepositoryPackagesLocalOnlyCommits(t *testing.T) {
	workDir, remoteDir := initBundleTestRepository(t)
	baseCommit := headCommit(t, workDir)
	commitFile(t, workDir, "local.txt", "local\n", "local commit")
	localCommit := headCommit(t, workDir)

	bundle, err := BuildFromRepository(workDir, "origin", &repositorycheckout.Checkout{CommitSHA: localCommit})
	if err != nil {
		t.Fatalf("BuildFromRepository returned error: %v", err)
	}
	if bundle == nil {
		t.Fatal("expected local-only commit bundle")
	}
	if got, want := bundle.Format, FormatGitBundleV1; got != want {
		t.Fatalf("bundle format = %q, want %q", got, want)
	}
	if got, want := bundle.TargetCommitSHA, localCommit; got != want {
		t.Fatalf("target commit = %q, want %q", got, want)
	}
	if err := bundle.ValidateContent(); err != nil {
		t.Fatalf("ValidateContent returned error: %v", err)
	}
	prerequisites, err := bundle.PrerequisiteCommits()
	if err != nil {
		t.Fatalf("PrerequisiteCommits returned error: %v", err)
	}
	if !slices.Contains(prerequisites, baseCommit) {
		t.Fatalf("expected prerequisite %q, got %v", baseCommit, prerequisites)
	}
	if err := bundle.VerifyAgainstRepository(context.Background(), remoteDir); err != nil {
		t.Fatalf("VerifyAgainstRepository returned error: %v", err)
	}
}

func TestBuildFromRepositoryReturnsNilWithoutLocalOnlyCommits(t *testing.T) {
	workDir, _ := initBundleTestRepository(t)
	head := headCommit(t, workDir)

	bundle, err := BuildFromRepository(workDir, "origin", &repositorycheckout.Checkout{CommitSHA: head})
	if err != nil {
		t.Fatalf("BuildFromRepository returned error: %v", err)
	}
	if bundle != nil {
		t.Fatalf("expected no bundle, got %#v", bundle)
	}
}

func TestBuildFromRepositoryRejectsFullHistoryBundleWithoutPrerequisites(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(work) returned error: %v", err)
	}
	runGit(t, workDir, "init")
	runGit(t, workDir, "config", "user.name", "Cleanroom Test")
	runGit(t, workDir, "config", "user.email", "cleanroom-test@example.com")
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGit(t, workDir, "add", "README.md")
	runGit(t, workDir, "commit", "-m", "initial")
	runGit(t, workDir, "remote", "add", "origin", "https://github.com/buildkite/cleanroom.git")
	head := headCommit(t, workDir)

	bundle, err := BuildFromRepository(workDir, "origin", &repositorycheckout.Checkout{CommitSHA: head})
	if err == nil {
		t.Fatal("expected full-history bundle rejection")
	}
	if bundle != nil {
		t.Fatalf("expected rejected bundle to be nil, got %#v", bundle)
	}
	if !strings.Contains(err.Error(), "full-history bundles are not supported yet") {
		t.Fatalf("expected full-history rejection, got %v", err)
	}
}

func initBundleTestRepository(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	workDir := filepath.Join(root, "work")

	runGit(t, root, "init", "--bare", remoteDir)
	runGit(t, root, "clone", remoteDir, workDir)
	runGit(t, workDir, "config", "user.name", "Cleanroom Test")
	runGit(t, workDir, "config", "user.email", "cleanroom-test@example.com")
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGit(t, workDir, "add", "README.md")
	runGit(t, workDir, "commit", "-m", "initial")
	runGit(t, workDir, "branch", "-M", "main")
	runGit(t, workDir, "push", "-u", "origin", "main")

	return workDir, remoteDir
}

func commitFile(t *testing.T, dir, name, content, message string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	runGit(t, dir, "add", name)
	runGit(t, dir, "commit", "-m", message)
}

func headCommit(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(string(runGit(t, dir, "rev-parse", "HEAD")))
}

func runGit(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
	return out
}
