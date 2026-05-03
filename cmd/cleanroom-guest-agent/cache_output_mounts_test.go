//go:build linux

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/vsockexec"
	"golang.org/x/sys/unix"
)

func TestCacheOutputMountActionsMapsDirsAndRestoresFiles(t *testing.T) {
	t.Parallel()

	actions, err := cacheOutputMountActions([]vsockexec.CacheOutputMount{
		{
			DevicePath:    "/dev/vdb",
			MountPath:     "/run/cleanroom/cache-output-volumes/cacheout0",
			SourcePresent: true,
			DirMappings: []vsockexec.CacheOutputDirMount{
				{GuestPath: "/root/.local/share/mise", Subpath: "dirs/0"},
			},
			FileMappings: []vsockexec.CacheOutputFileMount{
				{GuestPath: "/root/.config/mise/config.toml", Subpath: "files/0", Mode: 0o600},
			},
		},
	})
	if err != nil {
		t.Fatalf("cacheOutputMountActions returned error: %v", err)
	}

	want := []cacheOutputMountAction{
		{Kind: cacheOutputActionMkdir, Target: "/run/cleanroom/cache-output-volumes/cacheout0", Mode: 0o755},
		{Kind: cacheOutputActionMount, Source: "/dev/vdb", Target: "/run/cleanroom/cache-output-volumes/cacheout0", FSType: "ext4"},
		{Kind: cacheOutputActionMkdir, Source: "/run/cleanroom/cache-output-volumes/cacheout0/dirs/0", VolumeRoot: "/run/cleanroom/cache-output-volumes/cacheout0", VolumeSubpath: "dirs/0", Mode: 0o755, RequireExisting: true},
		{Kind: cacheOutputActionMkdir, Target: "/root/.local/share/mise", Mode: 0o755, RequireEmpty: true},
		{Kind: cacheOutputActionBind, Source: "/run/cleanroom/cache-output-volumes/cacheout0/dirs/0", Target: "/root/.local/share/mise", VolumeRoot: "/run/cleanroom/cache-output-volumes/cacheout0", VolumeSubpath: "dirs/0", Flags: unix.MS_BIND},
		{Kind: cacheOutputActionMkdir, Target: "/root/.config/mise", Mode: 0o755},
		{Kind: cacheOutputActionRestoreFile, Source: "/run/cleanroom/cache-output-volumes/cacheout0/files/0", Target: "/root/.config/mise/config.toml", VolumeRoot: "/run/cleanroom/cache-output-volumes/cacheout0", VolumeSubpath: "files/0", Mode: 0o600, Required: true},
	}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("unexpected actions:\n got %#v\nwant %#v", actions, want)
	}
}

func TestEnsureCacheOutputDirRejectsNonEmptyMountpoint(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing"), []byte("image content"), 0o644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}

	err := ensureCacheOutputDir(dir, 0o755, false, true)
	if err == nil {
		t.Fatal("expected non-empty directory to fail")
	}
	if !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureCacheOutputDirRequiresExistingHitSource(t *testing.T) {
	t.Parallel()

	err := ensureCacheOutputDir(filepath.Join(t.TempDir(), "missing"), 0o755, true, false)
	if err == nil {
		t.Fatal("expected missing hit source directory to fail")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureCacheOutputVolumeDirRejectsSymlinkParent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "dirs")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	_, err := ensureCacheOutputVolumeDir(root, "dirs/0", 0o755, false)
	if err == nil {
		t.Fatal("expected symlink parent to fail")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOpenCacheOutputVolumeFileRejectsSymlinkFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "files"), 0o755); err != nil {
		t.Fatalf("create files dir: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "files", "0")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	file, _, err := openCacheOutputVolumeFile(root, "files/0", true)
	if file != nil {
		_ = file.Close()
	}
	if err == nil {
		t.Fatal("expected symlink file to fail")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCacheOutputMountActionsRejectsEscapingSubpath(t *testing.T) {
	t.Parallel()

	_, err := cacheOutputMountActions([]vsockexec.CacheOutputMount{
		{
			DevicePath: "/dev/vdb",
			MountPath:  "/run/cleanroom/cache-output-volumes/cacheout0",
			DirMappings: []vsockexec.CacheOutputDirMount{
				{GuestPath: "/root/.local/share/mise", Subpath: "../escape"},
			},
		},
	})
	if err == nil {
		t.Fatal("expected escaping subpath to fail")
	}
	if !strings.Contains(err.Error(), "escapes output volume") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRestoreCacheOutputFileCopiesHitAndSkipsMissingMiss(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "volume", "files", "0")
	targetPath := filepath.Join(dir, "root", "config.toml")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o755); err != nil {
		t.Fatalf("create source dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		t.Fatalf("create target dir: %v", err)
	}
	if err := os.WriteFile(sourcePath, []byte("restored"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	if err := restoreCacheOutputFile(sourcePath, targetPath, 0, true); err != nil {
		t.Fatalf("restoreCacheOutputFile returned error: %v", err)
	}
	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if got, want := string(data), "restored"; got != want {
		t.Fatalf("unexpected restored file content: got %q want %q", got, want)
	}
	if info, err := os.Stat(targetPath); err != nil {
		t.Fatalf("stat target: %v", err)
	} else if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("unexpected restored file mode: got %v want %v", got, want)
	}

	missingTarget := filepath.Join(dir, "root", "missing.toml")
	if err := restoreCacheOutputFile(filepath.Join(dir, "volume", "files", "missing"), missingTarget, 0, false); err != nil {
		t.Fatalf("restore missing non-required file returned error: %v", err)
	}
	if _, err := os.Stat(missingTarget); !os.IsNotExist(err) {
		t.Fatalf("expected missing non-required file not to be created, got err=%v", err)
	}
}

func TestRestoreCacheOutputFileReplacesExistingTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "volume", "files", "0")
	targetPath := filepath.Join(dir, "root", "config.toml")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o755); err != nil {
		t.Fatalf("create source dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		t.Fatalf("create target dir: %v", err)
	}
	if err := os.WriteFile(sourcePath, []byte("new"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.WriteFile(targetPath, []byte("old"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	if err := restoreCacheOutputFile(sourcePath, targetPath, 0o600, true); err != nil {
		t.Fatalf("restoreCacheOutputFile returned error: %v", err)
	}
	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if got, want := string(data), "new"; got != want {
		t.Fatalf("unexpected restored file content: got %q want %q", got, want)
	}
	if entries, err := os.ReadDir(filepath.Dir(targetPath)); err != nil {
		t.Fatalf("read target dir: %v", err)
	} else if len(entries) != 1 {
		t.Fatalf("expected only restored target in directory, got %d entries", len(entries))
	}
}

func TestSetupCacheOutputMountsOnceRejectsChangedPlan(t *testing.T) {
	cacheOutputMountState.Lock()
	previousSignature := cacheOutputMountState.signature
	cacheOutputMountState.signature = "first"
	cacheOutputMountState.Unlock()
	t.Cleanup(func() {
		cacheOutputMountState.Lock()
		cacheOutputMountState.signature = previousSignature
		cacheOutputMountState.Unlock()
	})

	err := setupCacheOutputMountsOnce([]vsockexec.CacheOutputMount{
		{
			DevicePath: "/dev/vdb",
			MountPath:  "/run/cleanroom/cache-output-volumes/cacheout0",
			DirMappings: []vsockexec.CacheOutputDirMount{
				{GuestPath: "/root/.local/share/mise", Subpath: "dirs/0"},
			},
		},
	})
	if err == nil {
		t.Fatal("expected changed mount plan to fail")
	}
	if !strings.Contains(err.Error(), "mount plan changed") {
		t.Fatalf("unexpected error: %v", err)
	}
}
