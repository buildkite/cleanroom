package controlservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitTreeEntriesForFilesReturnsCorrectModeAndType(t *testing.T) {
	repoDir := t.TempDir()
	runTestGit(t, repoDir, "init")
	runTestGit(t, repoDir, "config", "user.email", "test@example.com")
	runTestGit(t, repoDir, "config", "user.name", "Test User")

	if err := os.WriteFile(filepath.Join(repoDir, "regular.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write regular.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "executable.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write executable.sh: %v", err)
	}
	runTestGit(t, repoDir, "add", ".")
	runTestGit(t, repoDir, "commit", "-m", "init")
	commitSHA := strings.TrimSpace(runTestGit(t, repoDir, "rev-parse", "HEAD"))

	entries, err := gitTreeEntriesForFiles(context.Background(), repoDir, commitSHA, []string{"regular.txt", "executable.sh"})
	if err != nil {
		t.Fatalf("gitTreeEntriesForFiles returned error: %v", err)
	}

	if got, want := len(entries), 2; got != want {
		t.Fatalf("unexpected entry count: got %d want %d", got, want)
	}

	regular, ok := entries["regular.txt"]
	if !ok {
		t.Fatal("expected regular.txt in entries")
	}
	if got, want := regular.Mode, "100644"; got != want {
		t.Fatalf("unexpected mode for regular.txt: got %q want %q", got, want)
	}
	if got, want := regular.Type, "blob"; got != want {
		t.Fatalf("unexpected type for regular.txt: got %q want %q", got, want)
	}

	exec, ok := entries["executable.sh"]
	if !ok {
		t.Fatal("expected executable.sh in entries")
	}
	if got, want := exec.Mode, "100755"; got != want {
		t.Fatalf("unexpected mode for executable.sh: got %q want %q", got, want)
	}
	if got, want := exec.Type, "blob"; got != want {
		t.Fatalf("unexpected type for executable.sh: got %q want %q", got, want)
	}
}

func TestGitTreeEntriesForFilesMissingPathsAbsent(t *testing.T) {
	repoDir := t.TempDir()
	runTestGit(t, repoDir, "init")
	runTestGit(t, repoDir, "config", "user.email", "test@example.com")
	runTestGit(t, repoDir, "config", "user.name", "Test User")

	if err := os.WriteFile(filepath.Join(repoDir, "exists.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatalf("write exists.txt: %v", err)
	}
	runTestGit(t, repoDir, "add", ".")
	runTestGit(t, repoDir, "commit", "-m", "init")
	commitSHA := strings.TrimSpace(runTestGit(t, repoDir, "rev-parse", "HEAD"))

	entries, err := gitTreeEntriesForFiles(context.Background(), repoDir, commitSHA, []string{"exists.txt", "missing.txt"})
	if err != nil {
		t.Fatalf("gitTreeEntriesForFiles returned error: %v", err)
	}

	if _, ok := entries["exists.txt"]; !ok {
		t.Fatal("expected exists.txt in entries")
	}
	if _, ok := entries["missing.txt"]; ok {
		t.Fatal("expected missing.txt to be absent from entries")
	}
}

func TestGitTreeEntriesForFilesSymlink(t *testing.T) {
	repoDir := t.TempDir()
	runTestGit(t, repoDir, "init")
	runTestGit(t, repoDir, "config", "user.email", "test@example.com")
	runTestGit(t, repoDir, "config", "user.name", "Test User")

	if err := os.WriteFile(filepath.Join(repoDir, "target.txt"), []byte("target\n"), 0o644); err != nil {
		t.Fatalf("write target.txt: %v", err)
	}
	if err := os.Symlink("target.txt", filepath.Join(repoDir, "link.txt")); err != nil {
		t.Fatalf("symlink link.txt: %v", err)
	}
	runTestGit(t, repoDir, "add", ".")
	runTestGit(t, repoDir, "commit", "-m", "init")
	commitSHA := strings.TrimSpace(runTestGit(t, repoDir, "rev-parse", "HEAD"))

	entries, err := gitTreeEntriesForFiles(context.Background(), repoDir, commitSHA, []string{"link.txt"})
	if err != nil {
		t.Fatalf("gitTreeEntriesForFiles returned error: %v", err)
	}

	link, ok := entries["link.txt"]
	if !ok {
		t.Fatal("expected link.txt in entries")
	}
	if got, want := link.Mode, "120000"; got != want {
		t.Fatalf("unexpected mode for link.txt: got %q want %q", got, want)
	}
	if got, want := link.Type, "blob"; got != want {
		t.Fatalf("unexpected type for link.txt: got %q want %q", got, want)
	}
}

func TestGitFileDigestsAtCommitMatchesSHA256(t *testing.T) {
	repoDir := t.TempDir()
	runTestGit(t, repoDir, "init")
	runTestGit(t, repoDir, "config", "user.email", "test@example.com")
	runTestGit(t, repoDir, "config", "user.name", "Test User")

	files := map[string][]byte{
		"alpha.txt": []byte("alpha content\n"),
		"beta.txt":  []byte("beta content\n"),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repoDir, name), content, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	runTestGit(t, repoDir, "add", ".")
	runTestGit(t, repoDir, "commit", "-m", "init")
	commitSHA := strings.TrimSpace(runTestGit(t, repoDir, "rev-parse", "HEAD"))

	digests, err := gitFileDigestsAtCommit(context.Background(), repoDir, commitSHA, []string{"alpha.txt", "beta.txt"})
	if err != nil {
		t.Fatalf("gitFileDigestsAtCommit returned error: %v", err)
	}

	for name, content := range files {
		sum := sha256.Sum256(content)
		want := "sha256:" + hex.EncodeToString(sum[:])
		if got := digests[name]; got != want {
			t.Fatalf("unexpected digest for %s: got %q want %q", name, got, want)
		}
	}
}

func TestGitFileDigestsAtCommitEmptyBlob(t *testing.T) {
	repoDir := t.TempDir()
	runTestGit(t, repoDir, "init")
	runTestGit(t, repoDir, "config", "user.email", "test@example.com")
	runTestGit(t, repoDir, "config", "user.name", "Test User")

	if err := os.WriteFile(filepath.Join(repoDir, "empty.txt"), []byte{}, 0o644); err != nil {
		t.Fatalf("write empty.txt: %v", err)
	}
	runTestGit(t, repoDir, "add", ".")
	runTestGit(t, repoDir, "commit", "-m", "init")
	commitSHA := strings.TrimSpace(runTestGit(t, repoDir, "rev-parse", "HEAD"))

	digests, err := gitFileDigestsAtCommit(context.Background(), repoDir, commitSHA, []string{"empty.txt"})
	if err != nil {
		t.Fatalf("gitFileDigestsAtCommit returned error: %v", err)
	}

	sum := sha256.Sum256([]byte{})
	want := "sha256:" + hex.EncodeToString(sum[:])
	if got := digests["empty.txt"]; got != want {
		t.Fatalf("unexpected digest for empty.txt: got %q want %q", got, want)
	}
}

func TestGitFileDigestsAtCommitEmbeddedNewlines(t *testing.T) {
	repoDir := t.TempDir()
	runTestGit(t, repoDir, "init")
	runTestGit(t, repoDir, "config", "user.email", "test@example.com")
	runTestGit(t, repoDir, "config", "user.name", "Test User")

	content := []byte("line one\nline two\nline three\n\nafter blank\n")
	if err := os.WriteFile(filepath.Join(repoDir, "multiline.txt"), content, 0o644); err != nil {
		t.Fatalf("write multiline.txt: %v", err)
	}
	runTestGit(t, repoDir, "add", ".")
	runTestGit(t, repoDir, "commit", "-m", "init")
	commitSHA := strings.TrimSpace(runTestGit(t, repoDir, "rev-parse", "HEAD"))

	digests, err := gitFileDigestsAtCommit(context.Background(), repoDir, commitSHA, []string{"multiline.txt"})
	if err != nil {
		t.Fatalf("gitFileDigestsAtCommit returned error: %v", err)
	}

	sum := sha256.Sum256(content)
	want := "sha256:" + hex.EncodeToString(sum[:])
	if got := digests["multiline.txt"]; got != want {
		t.Fatalf("unexpected digest for multiline.txt: got %q want %q", got, want)
	}
}

func TestGitFileDigestsAtCommitMissingObject(t *testing.T) {
	repoDir := t.TempDir()
	runTestGit(t, repoDir, "init")
	runTestGit(t, repoDir, "config", "user.email", "test@example.com")
	runTestGit(t, repoDir, "config", "user.name", "Test User")

	if err := os.WriteFile(filepath.Join(repoDir, "exists.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatalf("write exists.txt: %v", err)
	}
	runTestGit(t, repoDir, "add", ".")
	runTestGit(t, repoDir, "commit", "-m", "init")
	commitSHA := strings.TrimSpace(runTestGit(t, repoDir, "rev-parse", "HEAD"))

	_, err := gitFileDigestsAtCommit(context.Background(), repoDir, commitSHA, []string{"nothere.txt"})
	if err == nil {
		t.Fatal("expected error for missing object")
	}
	if !strings.Contains(err.Error(), "is missing") {
		t.Fatalf("expected 'is missing' in error, got: %v", err)
	}
}

func TestGitFileDigestsAtCommitStress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	repoDir := t.TempDir()
	runTestGit(t, repoDir, "init")
	runTestGit(t, repoDir, "config", "user.email", "test@example.com")
	runTestGit(t, repoDir, "config", "user.name", "Test User")

	const fileCount = 500
	fileNames := make([]string, fileCount)
	fileContents := make(map[string][]byte, fileCount)
	for i := 0; i < fileCount; i++ {
		name := fmt.Sprintf("file%04d.txt", i)
		content := []byte(fmt.Sprintf("content of file %d\n", i))
		if err := os.WriteFile(filepath.Join(repoDir, name), content, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		fileNames[i] = name
		fileContents[name] = content
	}
	runTestGit(t, repoDir, "add", ".")
	runTestGit(t, repoDir, "commit", "-m", "stress")
	commitSHA := strings.TrimSpace(runTestGit(t, repoDir, "rev-parse", "HEAD"))

	digests, err := gitFileDigestsAtCommit(context.Background(), repoDir, commitSHA, fileNames)
	if err != nil {
		t.Fatalf("gitFileDigestsAtCommit returned error: %v", err)
	}

	if got, want := len(digests), fileCount; got != want {
		t.Fatalf("unexpected digest count: got %d want %d", got, want)
	}

	for name, content := range fileContents {
		sum := sha256.Sum256(content)
		want := "sha256:" + hex.EncodeToString(sum[:])
		if got := digests[name]; got != want {
			t.Fatalf("unexpected digest for %s: got %q want %q", name, got, want)
		}
	}
}
