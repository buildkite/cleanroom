package submodule

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initSubmoduleRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Cleanroom Test")
	runGit(t, dir, "config", "user.email", "cleanroom-test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "initial")
	return dir
}

func initSuperproject(t *testing.T, submodulePaths map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Cleanroom Test")
	runGit(t, dir, "config", "user.email", "cleanroom-test@example.com")
	for smDir, smPath := range submodulePaths {
		runGitWithEnv(t, dir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "add", smDir, smPath)
	}
	runGit(t, dir, "commit", "-m", "add submodules")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return runGitWithEnv(t, dir, nil, args...)
}

func runGitWithEnv(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
	return string(out)
}

func TestListWorktreeSubmodules(t *testing.T) {
	emojisDir := initSubmoduleRepo(t)
	libsDir := initSubmoduleRepo(t)

	superDir := t.TempDir()
	runGit(t, superDir, "init")
	runGit(t, superDir, "config", "user.name", "Cleanroom Test")
	runGit(t, superDir, "config", "user.email", "cleanroom-test@example.com")
	runGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "add", emojisDir, "vendor/emojis")
	runGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "add", libsDir, "vendor/libs")
	runGit(t, superDir, "commit", "-m", "add submodules")

	subs, err := ListWorktreeSubmodules(superDir)
	if err != nil {
		t.Fatalf("ListWorktreeSubmodules returned error: %v", err)
	}
	if got, want := len(subs), 2; got != want {
		t.Fatalf("expected %d submodules, got %d", want, got)
	}
	if got, want := subs[0].Path, "vendor/emojis"; got != want {
		t.Errorf("first path: got %q want %q", got, want)
	}
	if got, want := subs[0].WorktreeDir, filepath.Join(superDir, "vendor/emojis"); got != want {
		t.Errorf("first WorktreeDir: got %q want %q", got, want)
	}
	if got, want := subs[1].Path, "vendor/libs"; got != want {
		t.Errorf("second path: got %q want %q", got, want)
	}
	if got, want := subs[1].WorktreeDir, filepath.Join(superDir, "vendor/libs"); got != want {
		t.Errorf("second WorktreeDir: got %q want %q", got, want)
	}
}

func TestListWorktreeSubmodulesRejectsUninitialised(t *testing.T) {
	subDir := initSubmoduleRepo(t)
	superDir := t.TempDir()
	runGit(t, superDir, "init")
	runGit(t, superDir, "config", "user.name", "Cleanroom Test")
	runGit(t, superDir, "config", "user.email", "cleanroom-test@example.com")
	runGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "add", subDir, "vendor/sub")
	runGit(t, superDir, "commit", "-m", "add submodule")

	smGitDir := filepath.Join(superDir, "vendor/sub/.git")
	if err := os.RemoveAll(smGitDir); err != nil {
		t.Fatalf("remove submodule .git: %v", err)
	}

	_, err := ListWorktreeSubmodules(superDir)
	if err == nil {
		t.Fatal("expected error for uninitialised submodule")
	}
	if !strings.Contains(err.Error(), "is not initialised") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestListWorktreeSubmoduleFiles(t *testing.T) {
	subDir := initSubmoduleRepo(t)
	if err := os.WriteFile(filepath.Join(subDir, "emoji.json"), []byte(`{"emoji":"🎉"}`), 0o644); err != nil {
		t.Fatalf("write emoji.json: %v", err)
	}
	runGit(t, subDir, "add", "emoji.json")
	runGit(t, subDir, "commit", "-m", "add emoji.json")

	superDir := t.TempDir()
	runGit(t, superDir, "init")
	runGit(t, superDir, "config", "user.name", "Cleanroom Test")
	runGit(t, superDir, "config", "user.email", "cleanroom-test@example.com")
	runGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "add", subDir, "vendor/emojis")
	runGit(t, superDir, "commit", "-m", "add submodule")

	sm := WorktreeSubmodule{
		Path:        "vendor/emojis",
		WorktreeDir: filepath.Join(superDir, "vendor/emojis"),
	}
	files, err := ListWorktreeSubmoduleFiles(sm)
	if err != nil {
		t.Fatalf("ListWorktreeSubmoduleFiles returned error: %v", err)
	}

	wantPrefix := "vendor/emojis/"
	for _, f := range files {
		if !strings.HasPrefix(f, wantPrefix) {
			t.Errorf("file %q missing expected prefix %q", f, wantPrefix)
		}
	}

	found := false
	for _, f := range files {
		if f == "vendor/emojis/emoji.json" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected vendor/emojis/emoji.json in files, got %v", files)
	}
}

func TestFindSubmoduleForPath(t *testing.T) {
	subs := []WorktreeSubmodule{
		{Path: "vendor/emojis", WorktreeDir: "/repo/vendor/emojis"},
	}

	sm, ok := FindSubmoduleForPath("vendor/emojis/foo.txt", subs)
	if !ok {
		t.Fatal("expected match for vendor/emojis/foo.txt")
	}
	if got, want := sm.Path, "vendor/emojis"; got != want {
		t.Errorf("Path: got %q want %q", got, want)
	}

	_, ok = FindSubmoduleForPath("other/file.txt", subs)
	if ok {
		t.Error("expected no match for other/file.txt")
	}

	_, ok = FindSubmoduleForPath("vendor/emojis", subs)
	if ok {
		t.Error("expected no match for exact submodule path without trailing slash")
	}
}

func TestFindSubmoduleForPathNestedPicksDeepest(t *testing.T) {
	subs := []WorktreeSubmodule{
		{Path: "a", WorktreeDir: "/repo/a"},
		{Path: "a/b", WorktreeDir: "/repo/a/b"},
		{Path: "a/b/c", WorktreeDir: "/repo/a/b/c"},
	}

	sm, ok := FindSubmoduleForPath("a/b/c/file.go", subs)
	if !ok {
		t.Fatal("expected match")
	}
	if got, want := sm.Path, "a/b/c"; got != want {
		t.Errorf("Path: got %q want %q", got, want)
	}
}
