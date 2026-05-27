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

func TestParseSubmoduleStatusPathPreservesSpaces(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{
			name: "path with spaces and describe suffix",
			line: " 1234567890123456789012345678901234567890 vendor/with space (heads/main)",
			want: "vendor/with space",
		},
		{
			name: "path with spaces and no describe suffix",
			line: "+1234567890123456789012345678901234567890 vendor/with space",
			want: "vendor/with space",
		},
		{
			name: "simple path",
			line: " 1234567890123456789012345678901234567890 vendor/emojis (v1.0)",
			want: "vendor/emojis",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSubmoduleStatusPath(tc.line)
			if err != nil {
				t.Fatalf("parseSubmoduleStatusPath: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestResolveSubmoduleURL(t *testing.T) {
	cases := []struct {
		name      string
		parent    string
		submodule string
		want      string
		wantErr   bool
	}{
		{
			name:      "absolute https",
			parent:    "https://example.com/parent.git",
			submodule: "https://example.com/sister.git",
			want:      "https://example.com/sister.git",
		},
		{
			name:      "ssh-style absolute",
			parent:    "https://example.com/parent.git",
			submodule: "git@github.com:org/sister.git",
			want:      "git@github.com:org/sister.git",
		},
		{
			name:      "dot-slash relative",
			parent:    "https://example.com/parent.git",
			submodule: "./child.git",
			want:      "https://example.com/parent.git/child.git",
		},
		{
			name:      "dot-dot relative against https",
			parent:    "https://example.com/org/parent.git",
			submodule: "../sister.git",
			want:      "https://example.com/org/sister.git",
		},
		{
			name:      "two dot-dot segments",
			parent:    "https://example.com/team/org/parent.git",
			submodule: "../../sister.git",
			want:      "https://example.com/team/sister.git",
		},
		{
			name:      "ssh-form parent with relative",
			parent:    "git@github.com:org/parent.git",
			submodule: "../sister.git",
			want:      "git@github.com:org/sister.git",
		},
		{
			name:      "relative without parent errors",
			parent:    "",
			submodule: "../sister.git",
			wantErr:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveSubmoduleURL(tc.parent, tc.submodule)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveSubmoduleURL: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestResolveMirrorSubmoduleURL(t *testing.T) {
	cases := []struct {
		name      string
		parent    string
		submodule string
		want      string
		wantErr   string
	}{
		{
			name:      "relative same host",
			parent:    "https://github.com/buildkite/cleanroom.git",
			submodule: "../tooling.git",
			want:      "https://github.com/buildkite/tooling.git",
		},
		{
			name:      "ssh form same host",
			parent:    "https://github.com/buildkite/cleanroom.git",
			submodule: "git@github.com:buildkite/tooling.git",
			want:      "https://github.com/buildkite/tooling.git",
		},
		{
			name:      "file rejected",
			parent:    "https://github.com/buildkite/cleanroom.git",
			submodule: "file:///private/repo.git",
			wantErr:   "must use https",
		},
		{
			name:      "http rejected",
			parent:    "https://github.com/buildkite/cleanroom.git",
			submodule: "http://github.com/buildkite/tooling.git",
			wantErr:   "must use https",
		},
		{
			name:      "different host rejected",
			parent:    "https://github.com/buildkite/cleanroom.git",
			submodule: "https://example.com/buildkite/tooling.git",
			wantErr:   "does not match parent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveMirrorSubmoduleURL(tc.parent, tc.submodule)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveMirrorSubmoduleURL: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}
