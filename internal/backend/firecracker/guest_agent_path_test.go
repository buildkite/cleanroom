package firecracker

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/buildkite/cleanroom/internal/runtimeassets"
)

func TestDiscoverGuestAgentBinaryPrefersInstalledLibexecBeforeSibling(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	selfDir := filepath.Join(tmp, "prefix", "bin")
	self := filepath.Join(selfDir, "cleanroom")
	sibling := filepath.Join(selfDir, "cleanroom-guest-agent-linux-amd64")
	libexec := filepath.Join(tmp, "prefix", "libexec", "cleanroom", "cleanroom-guest-agent-linux-amd64")
	if err := os.MkdirAll(selfDir, 0o755); err != nil {
		t.Fatalf("mkdir self dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(libexec), 0o755); err != nil {
		t.Fatalf("mkdir libexec dir: %v", err)
	}
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write self binary: %v", err)
	}
	if err := os.WriteFile(sibling, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write sibling guest agent: %v", err)
	}
	if err := os.WriteFile(libexec, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write libexec guest agent: %v", err)
	}

	got, err := discoverGuestAgentBinaryWith(
		"amd64",
		func(string) (string, error) { return "/usr/local/bin/cleanroom-guest-agent-linux-amd64", nil },
		func() (string, error) { return self, nil },
		func() (string, error) { return "", errors.New("no working directory") },
		os.Stat,
		func(path string) (bool, error) { return path == libexec, nil },
	)
	if err != nil {
		t.Fatalf("discoverGuestAgentBinaryWith returned error: %v", err)
	}
	if got != libexec {
		t.Fatalf("unexpected guest agent path: got %q want %q", got, libexec)
	}
}

func TestDiscoverGuestAgentBinaryUsesAncestorDistBeforePATH(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	repoRoot := filepath.Join(tmp, "repo")
	cwd := filepath.Join(repoRoot, "nested", "workdir")
	prebuilt := filepath.Join(repoRoot, "dist", "cleanroom-guest-agent-linux-amd64")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(prebuilt), 0o755); err != nil {
		t.Fatalf("mkdir dist dir: %v", err)
	}
	if err := os.WriteFile(prebuilt, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write prebuilt guest agent: %v", err)
	}

	got, err := discoverGuestAgentBinaryWith(
		"amd64",
		func(string) (string, error) { return "/usr/local/bin/cleanroom-guest-agent-linux-amd64", nil },
		func() (string, error) { return "", errors.New("no executable") },
		func() (string, error) { return cwd, nil },
		os.Stat,
		func(path string) (bool, error) { return path == prebuilt, nil },
	)
	if err != nil {
		t.Fatalf("discoverGuestAgentBinaryWith returned error: %v", err)
	}
	if got != prebuilt {
		t.Fatalf("unexpected guest agent path: got %q want %q", got, prebuilt)
	}
}

func TestDiscoverGuestAgentBinaryUsesStagedDistBeforeLegacyDistAndPATH(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	repoRoot := filepath.Join(tmp, "repo")
	cwd := filepath.Join(repoRoot, "nested", "workdir")
	staged := filepath.Join(repoRoot, "dist", runtimeassets.HostStageDirName(runtime.GOOS, "amd64"), "libexec", "cleanroom", "cleanroom-guest-agent-linux-amd64")
	legacy := filepath.Join(repoRoot, "dist", "cleanroom-guest-agent-linux-amd64")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		t.Fatalf("mkdir staged dist dir: %v", err)
	}
	if err := os.WriteFile(staged, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write staged guest agent: %v", err)
	}
	if err := os.WriteFile(legacy, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write legacy guest agent: %v", err)
	}

	got, err := discoverGuestAgentBinaryWith(
		"amd64",
		func(string) (string, error) { return "/usr/local/bin/cleanroom-guest-agent-linux-amd64", nil },
		func() (string, error) { return "", errors.New("no executable") },
		func() (string, error) { return cwd, nil },
		os.Stat,
		func(path string) (bool, error) { return path == staged, nil },
	)
	if err != nil {
		t.Fatalf("discoverGuestAgentBinaryWith returned error: %v", err)
	}
	if got != staged {
		t.Fatalf("unexpected guest agent path: got %q want %q", got, staged)
	}
}
