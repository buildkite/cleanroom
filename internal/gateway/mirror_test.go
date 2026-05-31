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

func TestGitMirrorStoreRefreshMirrorFetchesWithinMaxAge(t *testing.T) {
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

	mirrorDir2, err := store.RefreshMirror(context.Background(), "file://"+originDir)
	if err != nil {
		t.Fatalf("refresh mirror: %v", err)
	}
	if mirrorDir2 != mirrorDir {
		t.Fatalf("mirror directory changed: got %q want %q", mirrorDir2, mirrorDir)
	}

	got := gitRevParse(t, mirrorDir, "refs/heads/main")
	if got != head2 {
		t.Fatalf("expected mirror head %q after refresh, got %q", head2, got)
	}
}

func TestGitMirrorStoreRefreshMirrorUpdatesDefaultBranchHEAD(t *testing.T) {
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
	if got, want := strings.TrimSpace(runGitCommand(t, mirrorDir, "symbolic-ref", "--short", "HEAD")), "main"; got != want {
		t.Fatalf("unexpected initial mirror HEAD: got %q want %q", got, want)
	}

	runGitCommand(t, workDir, "checkout", "-b", "newdefault")
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("two\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGitCommand(t, workDir, "add", "README.md")
	runGitCommand(t, workDir, "commit", "-m", "new default")
	runGitCommand(t, workDir, "push", "-u", "origin", "newdefault")
	runGitCommand(t, originDir, "symbolic-ref", "HEAD", "refs/heads/newdefault")

	if _, err := store.RefreshMirror(context.Background(), "file://"+originDir); err != nil {
		t.Fatalf("refresh mirror: %v", err)
	}
	if got, want := strings.TrimSpace(runGitCommand(t, mirrorDir, "symbolic-ref", "--short", "HEAD")), "newdefault"; got != want {
		t.Fatalf("unexpected refreshed mirror HEAD: got %q want %q", got, want)
	}
}

func TestGitMirrorStoreRefreshMirrorSerializesWithEnsureMirror(t *testing.T) {
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
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		_, err := store.EnsureMirror(context.Background(), "file://"+originDir)
		errs <- err
	}()
	go func() {
		<-start
		_, err := store.RefreshMirror(context.Background(), "file://"+originDir)
		errs <- err
	}()
	close(start)

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent mirror operation returned error: %v", err)
		}
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
	env, err := store.gitEnvWithAuth(context.Background(), "https://github.com/buildkite/cleanroom.git", []string{
		"PATH=/bin",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url.https://github.com/.insteadOf",
		"GIT_CONFIG_VALUE_0=gh:",
	})
	if err != nil {
		t.Fatalf("git env with auth: %v", err)
	}

	if got, want := findEnvValue(env, "GIT_CONFIG_COUNT"), "2"; got != want {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want %q", got, want)
	}
	if got, want := findEnvValue(env, "GIT_CONFIG_KEY_0"), "url.https://github.com/.insteadOf"; got != want {
		t.Fatalf("GIT_CONFIG_KEY_0 = %q, want %q", got, want)
	}
	if got, want := findEnvValue(env, "GIT_CONFIG_VALUE_0"), "gh:"; got != want {
		t.Fatalf("GIT_CONFIG_VALUE_0 = %q, want %q", got, want)
	}
	if got, want := findEnvValue(env, "GIT_CONFIG_KEY_1"), "http.https://github.com/buildkite/cleanroom.git/.extraHeader"; got != want {
		t.Fatalf("GIT_CONFIG_KEY_1 = %q, want %q", got, want)
	}
	if got, want := findEnvValue(env, "GIT_CONFIG_VALUE_1"), "Authorization: Basic test"; got != want {
		t.Fatalf("GIT_CONFIG_VALUE_1 = %q, want %q", got, want)
	}
}

func TestGitMirrorStoreCredentialErrorFailsClosed(t *testing.T) {
	t.Parallel()

	store := NewGitMirrorStore(t.TempDir(), time.Minute, failingCredentialProvider{})
	_, err := store.gitEnvWithAuth(context.Background(), "https://github.com/buildkite/cleanroom.git", []string{"PATH=/bin"})
	if err == nil {
		t.Fatal("expected credential error")
	}
	if !strings.Contains(err.Error(), "resolve mirror credentials") {
		t.Fatalf("expected mirror credential context, got %v", err)
	}
}

func TestGitMirrorStoreSkipsCredentialsForFileRemote(t *testing.T) {
	t.Parallel()

	store := NewGitMirrorStore(t.TempDir(), time.Minute, failingCredentialProvider{})
	env, err := store.gitEnvWithAuth(context.Background(), "file:///tmp/origin.git", []string{"PATH=/bin"})
	if err != nil {
		t.Fatalf("git env with auth: %v", err)
	}
	if got, want := len(env), 1; got != want {
		t.Fatalf("env length = %d, want %d: %v", got, want, env)
	}
	if got, want := env[0], "PATH=/bin"; got != want {
		t.Fatalf("env[0] = %q, want %q", got, want)
	}
}

func TestGitMirrorStoreCloneMirrorBoundsCommandOutput(t *testing.T) {
	installLargeOutputGit(t, "clone")

	store := NewGitMirrorStore(t.TempDir(), time.Minute, nil)
	mirrorDir := filepath.Join(t.TempDir(), "mirror.git")
	err := store.cloneMirror(context.Background(), "https://github.com/buildkite/cleanroom.git", mirrorDir)
	assertBoundedGitOutputError(t, err)
	if _, statErr := os.Stat(mirrorDir); !os.IsNotExist(statErr) {
		t.Fatalf("failed clone should remove mirror dir, stat err = %v", statErr)
	}
}

func TestGitMirrorStoreFetchMirrorBoundsSetURLOutput(t *testing.T) {
	installLargeOutputGit(t, "set-url")

	store := NewGitMirrorStore(t.TempDir(), time.Minute, nil)
	err := store.fetchMirror(context.Background(), "https://github.com/buildkite/cleanroom.git", t.TempDir())
	assertBoundedGitOutputError(t, err)
	if !strings.Contains(err.Error(), "git remote set-url origin") {
		t.Fatalf("expected set-url context, got %q", err.Error())
	}
}

func TestGitMirrorStoreFetchMirrorBoundsFetchOutput(t *testing.T) {
	installLargeOutputGit(t, "fetch")

	store := NewGitMirrorStore(t.TempDir(), time.Minute, nil)
	err := store.fetchMirror(context.Background(), "https://github.com/buildkite/cleanroom.git", t.TempDir())
	assertBoundedGitOutputError(t, err)
	if !strings.Contains(err.Error(), "git fetch --prune origin") {
		t.Fatalf("expected fetch context, got %q", err.Error())
	}
}

func findEnvValue(env []string, name string) string {
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == name {
			return value
		}
	}
	return ""
}

type staticAuthorizationProvider struct {
	headers map[string]string
}

func (p staticAuthorizationProvider) Resolve(_ context.Context, remoteURL string) (string, error) {
	return p.headers[remoteURL], nil
}

type failingCredentialProvider struct{}

func (failingCredentialProvider) Resolve(context.Context, string) (string, error) {
	return "", fmt.Errorf("credentials unavailable")
}

func installLargeOutputGit(t *testing.T, failMode string) {
	t.Helper()

	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	script := `#!/bin/sh
emit_large_output() {
	dd if=/dev/zero bs=70000 count=1 2>/dev/null | LC_ALL=C tr '\000' x
	printf 'unretained-tail\n'
}

if [ "$FAKE_GIT_FAIL_MODE" = "clone" ] && [ "$1" = "clone" ]; then
	emit_large_output
	exit 1
fi

if [ "$FAKE_GIT_FAIL_MODE" = "set-url" ] && [ "$3" = "remote" ] && [ "$4" = "set-url" ]; then
	emit_large_output
	exit 1
fi

if [ "$FAKE_GIT_FAIL_MODE" = "fetch" ] && [ "$3" = "fetch" ]; then
	emit_large_output
	exit 1
fi

exit 0
`
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GIT_FAIL_MODE", failMode)
}

func assertBoundedGitOutputError(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		t.Fatal("expected git command error")
	}
	msg := err.Error()
	if len(msg) > maxGitCommandErrorOutputBytes+2048 {
		t.Fatalf("error output was not bounded: len=%d", len(msg))
	}
	if !strings.Contains(msg, "[truncated ") {
		t.Fatalf("expected truncation marker in bounded error output, len=%d", len(msg))
	}
	if strings.Contains(msg, "unretained-tail") {
		t.Fatalf("expected tail marker to be truncated, len=%d", len(msg))
	}
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
