package darwinvz

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		false,
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
		false,
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
		false,
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
		false,
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
		true,
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

func TestResolveHelperBinaryPathSkipsAncestorDistUnlessAllowed(t *testing.T) {
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
		false,
		func(string) (string, error) { return "/usr/local/bin/cleanroom-darwin-vz", nil },
		func() (string, error) { return "", errors.New("no executable") },
		func() (string, error) { return cwd, nil },
		os.Stat,
	)
	if err != nil {
		t.Fatalf("resolveHelperBinaryPathWith returned error: %v", err)
	}
	if got != "/usr/local/bin/cleanroom-darwin-vz" {
		t.Fatalf("unexpected helper path: got %q", got)
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
		true,
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
		true,
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
		true,
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
		false,
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
		false,
		func(string) (string, error) { return "", errors.New("not found") },
		func() (string, error) { return "", errors.New("no executable") },
		func() (string, error) { return "", errors.New("no working directory") },
		func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
	)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"cleanroom-darwin-vz", "CLEANROOM_DARWIN_VZ_HELPER", "CLEANROOM_DARWIN_VZ_HELPER_ALLOW_CWD", "mise run build"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to contain %q, got: %v", want, err)
		}
	}
}

func TestHelperWorkdirLookupAllowedParsesExplicitOptIn(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"1", "true", "TRUE", "yes", "on", " On "} {
		if !helperWorkdirLookupAllowed(value) {
			t.Fatalf("expected %q to enable workdir helper lookup", value)
		}
	}
	for _, value := range []string{"", "0", "false", "no", "off", "random"} {
		if helperWorkdirLookupAllowed(value) {
			t.Fatalf("expected %q to disable workdir helper lookup", value)
		}
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
