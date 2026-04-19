package repositorystore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/gateway"
)

func TestMirrorBackedRepositoryStoreEnsureCommitFetchesRequestedCommit(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	originDir := filepath.Join(t.TempDir(), "origin.git")

	runGitCommand(t, "", "init", "--bare", originDir)
	runGitCommand(t, workDir, "init")
	runGitCommand(t, workDir, "config", "user.email", "test@example.com")
	runGitCommand(t, workDir, "config", "user.name", "Test")
	runGitCommand(t, workDir, "branch", "-M", "main")
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGitCommand(t, workDir, "add", "README.md")
	runGitCommand(t, workDir, "commit", "-m", "initial")
	runGitCommand(t, workDir, "remote", "add", "origin", originDir)
	runGitCommand(t, workDir, "push", "-u", "origin", "main")

	mirrors := gateway.NewGitMirrorStore(t.TempDir(), time.Hour, nil)
	store := NewMirrorBacked(mirrors)

	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("two\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGitCommand(t, workDir, "add", "README.md")
	runGitCommand(t, workDir, "commit", "-m", "second")
	runGitCommand(t, workDir, "push", "origin", "main")
	head := strings.TrimSpace(runGitCommand(t, workDir, "rev-parse", "HEAD"))

	if err := store.EnsureCommit(context.Background(), "file://"+originDir, head, FetchHints{}); err != nil {
		t.Fatalf("ensure commit: %v", err)
	}

	if err := store.WithRepository(context.Background(), "file://"+originDir, head, FetchHints{}, func(repoDir string) error {
		got := strings.TrimSpace(runGitCommand(t, repoDir, "rev-parse", "refs/heads/main"))
		if got != head {
			t.Fatalf("unexpected mirror head: got %q want %q", got, head)
		}
		return nil
	}); err != nil {
		t.Fatalf("with repository: %v", err)
	}
}

func TestMirrorBackedRepositoryStoreReadFileAtCommit(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	originDir := filepath.Join(t.TempDir(), "origin.git")

	runGitCommand(t, "", "init", "--bare", originDir)
	runGitCommand(t, workDir, "init")
	runGitCommand(t, workDir, "config", "user.email", "test@example.com")
	runGitCommand(t, workDir, "config", "user.name", "Test")
	runGitCommand(t, workDir, "branch", "-M", "main")
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGitCommand(t, workDir, "add", "README.md")
	runGitCommand(t, workDir, "commit", "-m", "initial")
	runGitCommand(t, workDir, "remote", "add", "origin", originDir)
	runGitCommand(t, workDir, "push", "-u", "origin", "main")
	head := strings.TrimSpace(runGitCommand(t, workDir, "rev-parse", "HEAD"))

	store := NewMirrorBacked(gateway.NewGitMirrorStore(t.TempDir(), 0, nil))
	content, err := store.ReadFileAtCommit(context.Background(), "file://"+originDir, head, "README.md")
	if err != nil {
		t.Fatalf("read file at commit: %v", err)
	}
	if got, want := string(content), "hello\n"; got != want {
		t.Fatalf("unexpected file content: got %q want %q", got, want)
	}
}

func runGitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
