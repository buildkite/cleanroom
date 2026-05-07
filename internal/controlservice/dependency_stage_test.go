package controlservice

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandStageKeyFilesAtCommitDoublestarGlob(t *testing.T) {
	repoDir := initControlServiceGitRepo(t)
	if err := os.MkdirAll(filepath.Join(repoDir, "vendor", "lib"), 0o755); err != nil {
		t.Fatalf("create vendor dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "vendor", "lib", "a.lock"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("write a.lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "vendor", "b.lock"), []byte("b\n"), 0o644); err != nil {
		t.Fatalf("write b.lock: %v", err)
	}
	runControlServiceGit(t, repoDir, "add", ".")
	runControlServiceGit(t, repoDir, "commit", "-m", "add vendor files")
	commitSHA := headControlServiceCommit(t, repoDir)

	expanded, err := expandStageKeyFilesAtCommit(context.Background(), repoDir, commitSHA, []string{"vendor/**"}, "dependency")
	if err != nil {
		t.Fatalf("expandStageKeyFilesAtCommit returned error: %v", err)
	}
	if got, want := len(expanded), 2; got != want {
		t.Fatalf("unexpected expanded count: got %d want %d", got, want)
	}
	joined := strings.Join(expanded, ",")
	if !strings.Contains(joined, "vendor/lib/a.lock") {
		t.Fatalf("expected vendor/lib/a.lock in results, got %v", expanded)
	}
	if !strings.Contains(joined, "vendor/b.lock") {
		t.Fatalf("expected vendor/b.lock in results, got %v", expanded)
	}
}

func TestExpandStageKeyFilesAtCommitDoublestarGlobNoMatch(t *testing.T) {
	repoDir := initControlServiceGitRepo(t)
	commitSHA := headControlServiceCommit(t, repoDir)

	_, err := expandStageKeyFilesAtCommit(context.Background(), repoDir, commitSHA, []string{"vendor/**"}, "dependency")
	if err == nil {
		t.Fatal("expected no-match error")
	}
	if !strings.Contains(err.Error(), "matched no files") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func initControlServiceGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runControlServiceGit(t, dir, "init")
	runControlServiceGit(t, dir, "config", "user.name", "Cleanroom Test")
	runControlServiceGit(t, dir, "config", "user.email", "cleanroom-test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runControlServiceGit(t, dir, "add", "README.md")
	runControlServiceGit(t, dir, "commit", "-m", "initial")
	return dir
}

func headControlServiceCommit(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD failed: %v\n%s", err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func runControlServiceGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}
