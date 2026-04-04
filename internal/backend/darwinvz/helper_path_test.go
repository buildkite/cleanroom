package darwinvz

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertSameResolvedPath(t *testing.T, got, want string) {
	t.Helper()
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("resolve got path %q: %v", got, err)
	}
	wantResolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("resolve want path %q: %v", want, err)
	}
	if gotResolved != wantResolved {
		t.Fatalf("unexpected helper path: got %q want %q", gotResolved, wantResolved)
	}
}

func TestResolveHelperBinaryPathPrefersEnvOverride(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	override := tmp + "/cleanroom-darwin-vz-override"
	if err := os.WriteFile(override, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write override helper: %v", err)
	}

	got, err := resolveHelperBinaryPathWith(
		override,
		func(string) (string, error) { return "", errors.New("not found") },
		func() (string, error) { return "", errors.New("no executable") },
		func() (string, error) { return "", errors.New("no working directory") },
		os.Stat,
	)
	if err != nil {
		t.Fatalf("resolveHelperBinaryPathWith returned error: %v", err)
	}
	assertSameResolvedPath(t, got, override)
}

func TestResolveHelperBinaryPathUsesSiblingBeforePath(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	self := tmp + "/cleanroom"
	sibling := tmp + "/cleanroom-darwin-vz"
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write self binary: %v", err)
	}
	if err := os.WriteFile(sibling, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write sibling helper: %v", err)
	}

	got, err := resolveHelperBinaryPathWith(
		"",
		func(string) (string, error) { return "/usr/local/bin/cleanroom-darwin-vz", nil },
		func() (string, error) { return self, nil },
		func() (string, error) { return "", errors.New("no working directory") },
		os.Stat,
	)
	if err != nil {
		t.Fatalf("resolveHelperBinaryPathWith returned error: %v", err)
	}
	assertSameResolvedPath(t, got, sibling)
}

func TestResolveHelperBinaryPathPrefersSiblingAppBundleOverLooseBinary(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	self := tmp + "/cleanroom"
	sibling := tmp + "/cleanroom-darwin-vz"
	appBundle := sibling + ".app"
	appExecutable := appBundle + "/Contents/MacOS/cleanroom-darwin-vz"
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write self binary: %v", err)
	}
	if err := os.WriteFile(sibling, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write sibling helper: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(appExecutable), 0o755); err != nil {
		t.Fatalf("mkdir app bundle: %v", err)
	}
	if err := os.WriteFile(appExecutable, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write app bundle helper: %v", err)
	}

	got, err := resolveHelperBinaryPathWith(
		"",
		func(string) (string, error) { return "/usr/local/bin/cleanroom-darwin-vz", nil },
		func() (string, error) { return self, nil },
		func() (string, error) { return "", errors.New("no working directory") },
		os.Stat,
	)
	if err != nil {
		t.Fatalf("resolveHelperBinaryPathWith returned error: %v", err)
	}
	assertSameResolvedPath(t, got, appExecutable)
}

func TestResolveHelperBinaryPathPrefersResolvedExecutableSiblingAppBundleOverSymlinkSiblingBinary(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	installDir := filepath.Join(tmp, "usr-local-bin")
	appHelpersDir := filepath.Join(tmp, "Applications", "Cleanroom.app", "Contents", "Helpers")
	selfSymlink := filepath.Join(installDir, "cleanroom")
	staleSibling := filepath.Join(installDir, "cleanroom-darwin-vz")
	appBundle := filepath.Join(appHelpersDir, "cleanroom-darwin-vz.app")
	appExecutable := filepath.Join(appBundle, "Contents", "MacOS", "cleanroom-darwin-vz")
	resolvedSelf := filepath.Join(appHelpersDir, "cleanroom")

	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatalf("mkdir install dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(appExecutable), 0o755); err != nil {
		t.Fatalf("mkdir app bundle: %v", err)
	}
	if err := os.WriteFile(resolvedSelf, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write resolved self binary: %v", err)
	}
	if err := os.Symlink(resolvedSelf, selfSymlink); err != nil {
		t.Fatalf("symlink self binary: %v", err)
	}
	if err := os.WriteFile(staleSibling, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write stale sibling helper: %v", err)
	}
	if err := os.WriteFile(appExecutable, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write app bundle helper: %v", err)
	}

	got, err := resolveHelperBinaryPathWith(
		"",
		func(string) (string, error) { return "/usr/local/bin/cleanroom-darwin-vz", nil },
		func() (string, error) { return selfSymlink, nil },
		func() (string, error) { return "", errors.New("no working directory") },
		os.Stat,
	)
	if err != nil {
		t.Fatalf("resolveHelperBinaryPathWith returned error: %v", err)
	}
	assertSameResolvedPath(t, got, appExecutable)
}

func TestResolveHelperBinaryPathUsesAppBundleOverride(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	appBundle := tmp + "/cleanroom-darwin-vz.app"
	executable := appBundle + "/Contents/MacOS/cleanroom-darwin-vz"
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatalf("mkdir app bundle: %v", err)
	}
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write app bundle helper: %v", err)
	}

	got, err := resolveHelperBinaryPathWith(
		appBundle,
		func(string) (string, error) { return "", errors.New("not found") },
		func() (string, error) { return "", errors.New("no executable") },
		func() (string, error) { return "", errors.New("no working directory") },
		os.Stat,
	)
	if err != nil {
		t.Fatalf("resolveHelperBinaryPathWith returned error: %v", err)
	}
	assertSameResolvedPath(t, got, executable)
}

func TestResolveHelperBinaryPathUsesAncestorDistBeforePATH(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	repoRoot := tmp + "/repo"
	cwd := repoRoot + "/nested/workdir"
	prebuilt := repoRoot + "/dist/cleanroom-darwin-vz"
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	if err := os.MkdirAll(repoRoot+"/dist", 0o755); err != nil {
		t.Fatalf("mkdir dist: %v", err)
	}
	if err := os.WriteFile(prebuilt, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write prebuilt helper: %v", err)
	}

	got, err := resolveHelperBinaryPathWith(
		"",
		func(string) (string, error) { return "/usr/local/bin/cleanroom-darwin-vz", nil },
		func() (string, error) { return "", errors.New("no executable") },
		func() (string, error) { return cwd, nil },
		os.Stat,
	)
	if err != nil {
		t.Fatalf("resolveHelperBinaryPathWith returned error: %v", err)
	}
	assertSameResolvedPath(t, got, prebuilt)
}

func TestResolveHelperBinaryPathUsesAncestorDistAppBundleBeforePATH(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	repoRoot := tmp + "/repo"
	cwd := repoRoot + "/nested/workdir"
	appBundle := repoRoot + "/dist/cleanroom-darwin-vz.app"
	executable := appBundle + "/Contents/MacOS/cleanroom-darwin-vz"
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatalf("mkdir app bundle: %v", err)
	}
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write app bundle helper: %v", err)
	}

	got, err := resolveHelperBinaryPathWith(
		"",
		func(string) (string, error) { return "/usr/local/bin/cleanroom-darwin-vz", nil },
		func() (string, error) { return "", errors.New("no executable") },
		func() (string, error) { return cwd, nil },
		os.Stat,
	)
	if err != nil {
		t.Fatalf("resolveHelperBinaryPathWith returned error: %v", err)
	}
	assertSameResolvedPath(t, got, executable)
}

func TestResolveHelperBinaryPathPrefersAncestorDistAppBundleOverLooseBinary(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	repoRoot := tmp + "/repo"
	cwd := repoRoot + "/nested/workdir"
	looseBinary := repoRoot + "/dist/cleanroom-darwin-vz"
	appBundle := repoRoot + "/dist/cleanroom-darwin-vz.app"
	appExecutable := appBundle + "/Contents/MacOS/cleanroom-darwin-vz"
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(appExecutable), 0o755); err != nil {
		t.Fatalf("mkdir app bundle: %v", err)
	}
	if err := os.WriteFile(looseBinary, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write loose helper: %v", err)
	}
	if err := os.WriteFile(appExecutable, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write app bundle helper: %v", err)
	}

	got, err := resolveHelperBinaryPathWith(
		"",
		func(string) (string, error) { return "/usr/local/bin/cleanroom-darwin-vz", nil },
		func() (string, error) { return "", errors.New("no executable") },
		func() (string, error) { return cwd, nil },
		os.Stat,
	)
	if err != nil {
		t.Fatalf("resolveHelperBinaryPathWith returned error: %v", err)
	}
	assertSameResolvedPath(t, got, appExecutable)
}

func TestResolveHelperBinaryPathPrefersSiblingBeforeAncestorDist(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	repoRoot := tmp + "/repo"
	cwd := repoRoot + "/nested/workdir"
	prebuilt := repoRoot + "/dist/cleanroom-darwin-vz"
	selfDir := tmp + "/bin"
	self := selfDir + "/cleanroom"
	sibling := selfDir + "/cleanroom-darwin-vz"
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	if err := os.MkdirAll(repoRoot+"/dist", 0o755); err != nil {
		t.Fatalf("mkdir dist: %v", err)
	}
	if err := os.MkdirAll(selfDir, 0o755); err != nil {
		t.Fatalf("mkdir self dir: %v", err)
	}
	if err := os.WriteFile(prebuilt, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write prebuilt helper: %v", err)
	}
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write self binary: %v", err)
	}
	if err := os.WriteFile(sibling, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write sibling helper: %v", err)
	}

	got, err := resolveHelperBinaryPathWith(
		"",
		func(string) (string, error) { return "/usr/local/bin/cleanroom-darwin-vz", nil },
		func() (string, error) { return self, nil },
		func() (string, error) { return cwd, nil },
		os.Stat,
	)
	if err != nil {
		t.Fatalf("resolveHelperBinaryPathWith returned error: %v", err)
	}
	assertSameResolvedPath(t, got, sibling)
}

func TestResolveHelperBinaryPathFallsBackToPATH(t *testing.T) {
	t.Parallel()

	got, err := resolveHelperBinaryPathWith(
		"",
		func(string) (string, error) { return "/usr/local/bin/cleanroom-darwin-vz", nil },
		func() (string, error) { return "", errors.New("no executable") },
		func() (string, error) { return "", errors.New("no working directory") },
		func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
	)
	if err != nil {
		t.Fatalf("resolveHelperBinaryPathWith returned error: %v", err)
	}
	if got != "/usr/local/bin/cleanroom-darwin-vz" {
		t.Fatalf("unexpected helper path: got %q", got)
	}
}

func TestResolveHelperBinaryPathReturnsActionableError(t *testing.T) {
	t.Parallel()

	_, err := resolveHelperBinaryPathWith(
		"",
		func(string) (string, error) { return "", errors.New("not found") },
		func() (string, error) { return "", errors.New("no executable") },
		func() (string, error) { return "", errors.New("no working directory") },
		func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "cleanroom-darwin-vz") || !strings.Contains(got, "CLEANROOM_DARWIN_VZ_HELPER") || !strings.Contains(got, "mise run build") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolvePrebuiltBinaryPathFromWorkdirUsesAncestorDist(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	repoRoot := tmp + "/repo"
	cwd := repoRoot + "/a/b/c"
	prebuilt := repoRoot + "/dist/cleanroom-darwin-vz"
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	if err := os.MkdirAll(repoRoot+"/dist", 0o755); err != nil {
		t.Fatalf("mkdir dist dir: %v", err)
	}
	if err := os.WriteFile(prebuilt, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write repo dist helper: %v", err)
	}

	got, err := resolvePrebuiltBinaryPathFromWorkdir(cwd, helperBinaryName, os.Stat)
	if err != nil {
		t.Fatalf("resolvePrebuiltBinaryPathFromWorkdir returned error: %v", err)
	}
	if got != prebuilt {
		t.Fatalf("unexpected prebuilt path: got %q want %q", got, prebuilt)
	}
}
