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

func TestMirrorBackedRepositoryStoreWithRepositoryEnsuresMissingMirrorPath(t *testing.T) {
	t.Parallel()

	mirrorDir := filepath.Join(t.TempDir(), "mirror.git")
	mock := &mockMirrorSource{
		mirrorPath: mirrorDir,
		ensureMirrorFunc: func(_ context.Context, _ string) (string, error) {
			if err := os.MkdirAll(mirrorDir, 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(mirrorDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
				return "", err
			}
			return mirrorDir, nil
		},
	}
	store := NewMirrorBacked(mock)

	var gotRepoDir string
	err := store.WithRepository(context.Background(), "https://github.com/buildkite/cleanroom.git", "", FetchHints{}, func(repoDir string) error {
		gotRepoDir = repoDir
		return nil
	})
	if err != nil {
		t.Fatalf("WithRepository returned error: %v", err)
	}
	if gotRepoDir != mirrorDir {
		t.Fatalf("unexpected repo dir: got %q want %q", gotRepoDir, mirrorDir)
	}
	if mock.ensureMirrorCalls != 1 {
		t.Fatalf("expected EnsureMirror to be called once, got %d", mock.ensureMirrorCalls)
	}
}

func TestMirrorBackedRepositoryStoreWithRepositoryRefreshesExistingMirror(t *testing.T) {
	t.Parallel()

	mirrorDir := filepath.Join(t.TempDir(), "mirror.git")
	if err := os.MkdirAll(mirrorDir, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mirrorDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	mock := &mockMirrorSource{mirrorPath: mirrorDir}
	store := NewMirrorBacked(mock)

	err := store.WithRepository(context.Background(), "https://github.com/buildkite/cleanroom.git", "", FetchHints{}, func(repoDir string) error {
		if repoDir != mirrorDir {
			t.Fatalf("unexpected repo dir: got %q want %q", repoDir, mirrorDir)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithRepository returned error: %v", err)
	}
	if mock.ensureMirrorCalls != 1 {
		t.Fatalf("expected EnsureMirror to be called once, got %d", mock.ensureMirrorCalls)
	}
}

func TestEnsureSubmoduleMirror(t *testing.T) {
	t.Parallel()

	const wantRemoteURL = "https://github.com/example/submodule.git"
	const wantSHA = "abc1234abc1234abc1234abc1234abc1234abc12"
	const wantPath = "/mirrors/submodule"

	mock := &mockMirrorSource{mirrorPath: wantPath}
	store := NewMirrorBacked(mock)

	got, err := store.EnsureSubmoduleMirror(context.Background(), wantRemoteURL, wantSHA)
	if err != nil {
		t.Fatalf("EnsureSubmoduleMirror returned error: %v", err)
	}
	if got != wantPath {
		t.Fatalf("unexpected mirror dir: got %q want %q", got, wantPath)
	}
	if mock.ensureMirrorContainsURL != wantRemoteURL {
		t.Fatalf("unexpected EnsureMirrorContains remoteURL: got %q want %q", mock.ensureMirrorContainsURL, wantRemoteURL)
	}
	if mock.ensureMirrorContainsSHA != wantSHA {
		t.Fatalf("unexpected EnsureMirrorContains commitSHA: got %q want %q", mock.ensureMirrorContainsSHA, wantSHA)
	}
	if mock.mirrorPathURL != wantRemoteURL {
		t.Fatalf("unexpected MirrorPath remoteURL: got %q want %q", mock.mirrorPathURL, wantRemoteURL)
	}
}

func TestEnsureSubmoduleMirrorRejectsLocalRemote(t *testing.T) {
	t.Parallel()

	store := NewMirrorBacked(&mockMirrorSource{mirrorPath: "/mirrors/submodule"})

	_, err := store.EnsureSubmoduleMirror(context.Background(), "file:///private/repo.git", "abc1234abc1234abc1234abc1234abc1234abc12")
	if err == nil {
		t.Fatal("expected file remote to be rejected")
	}
	if !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("expected canonicalization error, got %v", err)
	}
}

type mockMirrorSource struct {
	mirrorPath              string
	ensureMirrorContainsURL string
	ensureMirrorContainsSHA string
	mirrorPathURL           string
	ensureMirrorCalls       int
	ensureMirrorFunc        func(context.Context, string) (string, error)
}

func (m *mockMirrorSource) MirrorPath(remoteURL string) (string, error) {
	m.mirrorPathURL = remoteURL
	return m.mirrorPath, nil
}

func (m *mockMirrorSource) EnsureMirror(ctx context.Context, remoteURL string) (string, error) {
	m.ensureMirrorCalls++
	if m.ensureMirrorFunc != nil {
		return m.ensureMirrorFunc(ctx, remoteURL)
	}
	return m.mirrorPath, nil
}

func (m *mockMirrorSource) EnsureMirrorContains(_ context.Context, remoteURL, commitSHA string) error {
	m.ensureMirrorContainsURL = remoteURL
	m.ensureMirrorContainsSHA = commitSHA
	return nil
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
