package gateway

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitMirrorStoreClonesAndRefreshesRemote(t *testing.T) {
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

	store := NewGitMirrorStore(t.TempDir(), 0, nil)
	mirrorDir, err := store.EnsureMirror(context.Background(), "file://"+originDir)
	if err != nil {
		t.Fatalf("ensure mirror: %v", err)
	}
	head1 := gitRevParse(t, mirrorDir, "refs/heads/main")

	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("two\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGitCommand(t, workDir, "add", "README.md")
	runGitCommand(t, workDir, "commit", "-m", "second")
	runGitCommand(t, workDir, "push", "origin", "main")

	mirrorDir2, err := store.EnsureMirror(context.Background(), "file://"+originDir)
	if err != nil {
		t.Fatalf("refresh mirror: %v", err)
	}
	if mirrorDir2 != mirrorDir {
		t.Fatalf("mirror directory changed: got %q want %q", mirrorDir2, mirrorDir)
	}

	head2 := gitRevParse(t, mirrorDir, "refs/heads/main")
	if head2 == head1 {
		t.Fatalf("expected mirror to refresh, still at %s", head2)
	}
}

func TestGitMirrorStoreEnsureMirrorContainsFetchesRequestedCommit(t *testing.T) {
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

	store := NewGitMirrorStore(t.TempDir(), time.Hour, nil)
	mirrorDir, err := store.EnsureMirror(context.Background(), "file://"+originDir)
	if err != nil {
		t.Fatalf("ensure mirror: %v", err)
	}
	head1 := gitRevParse(t, mirrorDir, "refs/heads/main")

	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("two\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGitCommand(t, workDir, "add", "README.md")
	runGitCommand(t, workDir, "commit", "-m", "second")
	runGitCommand(t, workDir, "push", "origin", "main")
	head2 := strings.TrimSpace(runGitCommand(t, workDir, "rev-parse", "HEAD"))
	if head2 == head1 {
		t.Fatalf("expected a new commit after second push")
	}

	if err := store.EnsureMirrorContains(context.Background(), "file://"+originDir, head2); err != nil {
		t.Fatalf("ensure mirror contains: %v", err)
	}
	got := gitRevParse(t, mirrorDir, "refs/heads/main")
	if got != head2 {
		t.Fatalf("expected mirror head %q after ensure-mirror-contains, got %q", head2, got)
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

func gitRevParse(t *testing.T, repoDir string, rev string) string {
	t.Helper()

	out := runGitCommand(t, repoDir, "rev-parse", rev)
	return strings.TrimSpace(out)
}

func TestGitMirrorStoreUsesRemoteURLScopedAuthHeader(t *testing.T) {
	t.Parallel()

	store := NewGitMirrorStore(t.TempDir(), time.Minute, staticAuthorizationProvider{
		headers: map[string]string{
			"https://github.com/buildkite/cleanroom.git": "Basic test",
		},
	})
	key, value := store.authConfigEntry("https://github.com/buildkite/cleanroom.git")
	if got, want := key, "http.https://github.com/buildkite/cleanroom.git/.extraHeader"; got != want {
		t.Fatalf("unexpected config key: got %q want %q", got, want)
	}
	if got, want := value, "Authorization: Basic test"; got != want {
		t.Fatalf("unexpected config value: got %q want %q", got, want)
	}
}

type staticAuthorizationProvider struct {
	headers map[string]string
}

func (p staticAuthorizationProvider) Resolve(_ context.Context, remoteURL string) (string, error) {
	return p.headers[remoteURL], nil
}

func TestGitMirrorStorePathIsStableByRemoteURL(t *testing.T) {
	t.Parallel()

	store := NewGitMirrorStore(t.TempDir(), time.Minute, nil)
	a := store.mirrorPath("https://github.com/buildkite/cleanroom.git")
	b := store.mirrorPath("https://github.com/buildkite/cleanroom.git")
	c := store.mirrorPath("https://github.com/buildkite/other.git")

	if a != b {
		t.Fatalf("expected stable mirror path, got %q and %q", a, b)
	}
	if a == c {
		t.Fatalf("expected different mirror path for different remote URL, got %q", a)
	}
	if filepath.Ext(a) != ".git" {
		t.Fatalf("expected mirror path to end in .git, got %q", a)
	}
	if !strings.Contains(a, fmt.Sprintf("%c", filepath.Separator)) {
		t.Fatalf("expected mirror path to include directories, got %q", a)
	}
}
