//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestSetupInputProjectionCopiesDeclaredFilesAndSetsDir(t *testing.T) {
	restoreRoot := setTestInputProjectionRoot(t)
	sourceRoot := t.TempDir()
	defer restoreRoot()
	targetRoot := filepath.Join(inputProjectionRoot, "dependency", "go-modules")
	if err := os.MkdirAll(filepath.Join(sourceRoot, "cmd"), 0o755); err != nil {
		t.Fatalf("create source dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "go.mod"), []byte("module example.test/app\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "cmd", "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	req, cleanup, err := setupInputProjection(vsockexec.ExecRequest{
		Command: []string{"true"},
		InputProjection: &vsockexec.InputProjection{
			SourceRoot: sourceRoot,
			TargetRoot: targetRoot,
			Files:      []string{"go.mod", "cmd/*.go"},
		},
	})
	if err != nil {
		t.Fatalf("setupInputProjection returned error: %v", err)
	}
	defer cleanup()
	if got, want := req.Dir, targetRoot; got != want {
		t.Fatalf("unexpected command dir: got %q want %q", got, want)
	}
	if data, err := os.ReadFile(filepath.Join(targetRoot, "go.mod")); err != nil {
		t.Fatalf("read projected go.mod: %v", err)
	} else if got, want := string(data), "module example.test/app\n"; got != want {
		t.Fatalf("unexpected projected go.mod: got %q want %q", got, want)
	}
	if info, err := os.Stat(filepath.Join(targetRoot, "cmd", "main.go")); err != nil {
		t.Fatalf("stat projected main.go: %v", err)
	} else if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("unexpected projected mode: got %v want %v", got, want)
	} else if !info.ModTime().Equal(inputProjectionTimestamp) {
		t.Fatalf("unexpected projected mtime: got %s want %s", info.ModTime(), inputProjectionTimestamp)
	}
}

func TestExpandInputProjectionFilesSortsAndDedupes(t *testing.T) {
	t.Parallel()

	sourceRoot := t.TempDir()
	for _, name := range []string{"b.sum", "a.sum"} {
		if err := os.WriteFile(filepath.Join(sourceRoot, name), []byte(name), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	files, err := expandInputProjectionFiles(sourceRoot, []string{"*.sum", "a.sum"})
	if err != nil {
		t.Fatalf("expandInputProjectionFiles returned error: %v", err)
	}
	if want := []string{"a.sum", "b.sum"}; !slices.Equal(files, want) {
		t.Fatalf("unexpected files: got %v want %v", files, want)
	}
}

func TestInputProjectionRejectsSymlink(t *testing.T) {
	restoreRoot := setTestInputProjectionRoot(t)
	sourceRoot := t.TempDir()
	defer restoreRoot()
	targetRoot := filepath.Join(inputProjectionRoot, "dependency", "toolchains")
	if err := os.Symlink("/etc/passwd", filepath.Join(sourceRoot, "link.lock")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	_, cleanup, err := setupInputProjection(vsockexec.ExecRequest{
		Command: []string{"true"},
		InputProjection: &vsockexec.InputProjection{
			SourceRoot: sourceRoot,
			TargetRoot: targetRoot,
			Files:      []string{"link.lock"},
		},
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil {
		t.Fatal("expected symlink input to fail")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInputProjectionRejectsSymlinkedParent(t *testing.T) {
	restoreRoot := setTestInputProjectionRoot(t)
	sourceRoot := t.TempDir()
	escapeRoot := t.TempDir()
	defer restoreRoot()
	targetRoot := filepath.Join(inputProjectionRoot, "dependency", "toolchains")
	if err := os.WriteFile(filepath.Join(escapeRoot, "passwd"), []byte("not from workspace\n"), 0o644); err != nil {
		t.Fatalf("write escaped file: %v", err)
	}
	if err := os.Symlink(escapeRoot, filepath.Join(sourceRoot, "deps")); err != nil {
		t.Fatalf("create symlinked parent: %v", err)
	}

	_, cleanup, err := setupInputProjection(vsockexec.ExecRequest{
		Command: []string{"true"},
		InputProjection: &vsockexec.InputProjection{
			SourceRoot: sourceRoot,
			TargetRoot: targetRoot,
			Files:      []string{"deps/passwd"},
		},
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil {
		t.Fatal("expected symlinked parent input to fail")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targetRoot, "deps", "passwd")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("projected escaped file unexpectedly exists: %v", err)
	}
}

func setTestInputProjectionRoot(t *testing.T) func() {
	t.Helper()
	previous := inputProjectionRoot
	inputProjectionRoot = filepath.Join(t.TempDir(), "input-projections")
	return func() {
		inputProjectionRoot = previous
	}
}
