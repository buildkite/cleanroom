package darwinvz

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/runtimeassets"
)

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
	if got != override {
		t.Fatalf("unexpected helper path: got %q want %q", got, override)
	}
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
	if got != sibling {
		t.Fatalf("unexpected helper path: got %q want %q", got, sibling)
	}
}

func TestResolveHelperBinaryPathPrefersInstalledLibexecAppBundleBeforeSibling(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	selfDir := filepath.Join(tmp, "prefix", "bin")
	self := filepath.Join(selfDir, "cleanroom")
	sibling := filepath.Join(selfDir, "cleanroom-darwin-vz")
	appBundle := filepath.Join(tmp, "prefix", "libexec", "cleanroom", "cleanroom-darwin-vz.app")
	appExecutable := filepath.Join(appBundle, "Contents", "MacOS", "cleanroom-darwin-vz")
	if err := os.MkdirAll(selfDir, 0o755); err != nil {
		t.Fatalf("mkdir self dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(appExecutable), 0o755); err != nil {
		t.Fatalf("mkdir app bundle: %v", err)
	}
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write self binary: %v", err)
	}
	if err := os.WriteFile(sibling, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write sibling helper: %v", err)
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
	if got != appExecutable {
		t.Fatalf("unexpected helper path: got %q want %q", got, appExecutable)
	}
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
	if got != appExecutable {
		t.Fatalf("unexpected helper path: got %q want %q", got, appExecutable)
	}
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
	if got != executable {
		t.Fatalf("unexpected helper path: got %q want %q", got, executable)
	}
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
	if got != prebuilt {
		t.Fatalf("unexpected helper path: got %q want %q", got, prebuilt)
	}
}

func TestResolveHelperBinaryPathUsesStagedDistBeforeLegacyDistAndPATH(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	repoRoot := filepath.Join(tmp, "repo")
	cwd := filepath.Join(repoRoot, "nested", "workdir")
	stageDir := filepath.Join(repoRoot, "dist", runtimeassets.HostStageDirName(runtime.GOOS, runtime.GOARCH), "libexec", "cleanroom", "cleanroom-darwin-vz.app")
	stagedExecutable := filepath.Join(stageDir, "Contents", "MacOS", "cleanroom-darwin-vz")
	legacyExecutable := filepath.Join(repoRoot, "dist", "cleanroom-darwin-vz")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(stagedExecutable), 0o755); err != nil {
		t.Fatalf("mkdir staged app bundle: %v", err)
	}
	if err := os.WriteFile(stagedExecutable, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write staged helper: %v", err)
	}
	if err := os.WriteFile(legacyExecutable, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write legacy helper: %v", err)
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
	if got != stagedExecutable {
		t.Fatalf("unexpected helper path: got %q want %q", got, stagedExecutable)
	}
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
	if got != executable {
		t.Fatalf("unexpected helper path: got %q want %q", got, executable)
	}
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
	if got != appExecutable {
		t.Fatalf("unexpected helper path: got %q want %q", got, appExecutable)
	}
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
	if got != sibling {
		t.Fatalf("unexpected helper path: got %q want %q", got, sibling)
	}
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

func TestDistCandidatesUsesAncestorDist(t *testing.T) {
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

	candidates := runtimeassets.DistCandidates(func() (string, error) { return cwd, nil }, helperBinaryName)
	got, err := runtimeassets.ResolveFirstCandidate(candidates, os.Stat, nil)
	if err != nil {
		t.Fatalf("ResolveFirstCandidate returned error: %v", err)
	}
	if got != prebuilt {
		t.Fatalf("unexpected prebuilt path: got %q want %q", got, prebuilt)
	}
}
