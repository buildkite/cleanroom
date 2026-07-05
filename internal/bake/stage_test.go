package bake

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStageWorkspaceCopiesGitVisibleFileSet(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "cleanroom-test@example.com")
	runGit(t, repo, "config", "user.name", "Cleanroom Test")

	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write(".gitignore", ".env\nnode_modules/\n")
	write("main.go", "package main\n")
	write("sub/dir/file.txt", "nested\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "-m", "initial")

	// Untracked-but-not-ignored: part of the git-visible set (and dirty).
	write("untracked.txt", "new\n")
	// Ignored secrets and build output: never staged.
	write(".env", "SECRET=hunter2\n")
	write("node_modules/pkg/index.js", "junk\n")
	// The in-repo bake artifact: excluded like the dirty decision excludes it.
	write("repo.spore/manifest.json", "{}")

	staged, cleanup, err := StageWorkspace(repo, []string{"repo.spore"})
	if err != nil {
		t.Fatalf("stage workspace: %v", err)
	}
	defer cleanup()

	for _, want := range []string{".gitignore", "main.go", "sub/dir/file.txt", "untracked.txt"} {
		if _, err := os.Stat(filepath.Join(staged, want)); err != nil {
			t.Fatalf("expected %s to be staged: %v", want, err)
		}
	}
	for _, absent := range []string{".env", "node_modules", ".git", "repo.spore"} {
		if _, err := os.Stat(filepath.Join(staged, absent)); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be absent from staging, stat err = %v", absent, err)
		}
	}
}

func TestStageWorkspacePreservesModeAndSymlinks(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "cleanroom-test@example.com")
	runGit(t, repo, "config", "user.name", "Cleanroom Test")
	if err := os.WriteFile(filepath.Join(repo, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write run.sh: %v", err)
	}
	if err := os.Symlink("run.sh", filepath.Join(repo, "link.sh")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	runGit(t, repo, "add", "-A")

	staged, cleanup, err := StageWorkspace(repo, nil)
	if err != nil {
		t.Fatalf("stage workspace: %v", err)
	}
	defer cleanup()

	info, err := os.Stat(filepath.Join(staged, "run.sh"))
	if err != nil {
		t.Fatalf("stat staged run.sh: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("staged run.sh lost the executable bit: %v", info.Mode())
	}
	target, err := os.Readlink(filepath.Join(staged, "link.sh"))
	if err != nil {
		t.Fatalf("readlink staged link.sh: %v", err)
	}
	if target != "run.sh" {
		t.Fatalf("staged symlink target = %q, want run.sh", target)
	}
}

func TestStageWorkspaceSkipsDeletedTrackedFiles(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "cleanroom-test@example.com")
	runGit(t, repo, "config", "user.name", "Cleanroom Test")
	if err := os.WriteFile(filepath.Join(repo, "gone.txt"), []byte("bye\n"), 0o644); err != nil {
		t.Fatalf("write gone.txt: %v", err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "-m", "initial")
	if err := os.Remove(filepath.Join(repo, "gone.txt")); err != nil {
		t.Fatalf("remove gone.txt: %v", err)
	}

	staged, cleanup, err := StageWorkspace(repo, nil)
	if err != nil {
		t.Fatalf("stage workspace: %v", err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(staged, "gone.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted tracked file must not be resurrected, stat err = %v", err)
	}
}

func TestSanitizeRemoteStripsCredentials(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		want   string
	}{
		{
			name:   "https token",
			remote: "https://x-access-token:ghs_secret@github.com/org/repo.git",
			want:   "https://github.com/org/repo.git",
		},
		{
			name:   "https user only",
			remote: "https://ci@github.com/org/repo.git",
			want:   "https://github.com/org/repo.git",
		},
		{
			name:   "plain https",
			remote: "https://github.com/org/repo.git",
			want:   "https://github.com/org/repo.git",
		},
		{
			name:   "scp style unchanged",
			remote: "git@github.com:org/repo.git",
			want:   "git@github.com:org/repo.git",
		},
		{
			name:   "ssh url user stripped",
			remote: "ssh://git@github.com/org/repo.git",
			want:   "ssh://github.com/org/repo.git",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeRemote(tc.remote); got != tc.want {
				t.Fatalf("sanitizeRemote(%q) = %q, want %q", tc.remote, got, tc.want)
			}
		})
	}
}
