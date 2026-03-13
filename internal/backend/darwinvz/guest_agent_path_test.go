//go:build darwin

package darwinvz

import (
	"errors"
	"os"
	"testing"
)

func TestDiscoverGuestAgentBinaryUsesAncestorDistBeforePATH(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	repoRoot := tmp + "/repo"
	cwd := repoRoot + "/nested/workdir"
	prebuilt := repoRoot + "/dist/cleanroom-guest-agent-linux-arm64"
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	if err := os.MkdirAll(repoRoot+"/dist", 0o755); err != nil {
		t.Fatalf("mkdir dist: %v", err)
	}
	if err := os.WriteFile(prebuilt, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write prebuilt guest agent: %v", err)
	}

	got, err := discoverGuestAgentBinaryWith(
		"arm64",
		func(string) (string, error) { return "/usr/local/bin/cleanroom-guest-agent-linux-arm64", nil },
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

func TestDiscoverGuestAgentBinaryUsesSiblingBeforePATH(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	self := tmp + "/cleanroom"
	sibling := tmp + "/cleanroom-guest-agent-linux-arm64"
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write self binary: %v", err)
	}
	if err := os.WriteFile(sibling, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write sibling guest agent: %v", err)
	}

	got, err := discoverGuestAgentBinaryWith(
		"arm64",
		func(string) (string, error) { return "/usr/local/bin/cleanroom-guest-agent-linux-arm64", nil },
		func() (string, error) { return self, nil },
		func() (string, error) { return "", errors.New("no working directory") },
		os.Stat,
		func(path string) (bool, error) { return path == sibling, nil },
	)
	if err != nil {
		t.Fatalf("discoverGuestAgentBinaryWith returned error: %v", err)
	}
	if got != sibling {
		t.Fatalf("unexpected guest agent path: got %q want %q", got, sibling)
	}
}

func TestDiscoverGuestAgentBinaryPrefersSiblingBeforeAncestorDist(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	repoRoot := tmp + "/repo"
	cwd := repoRoot + "/nested/workdir"
	prebuilt := repoRoot + "/dist/cleanroom-guest-agent-linux-arm64"
	selfDir := tmp + "/bin"
	self := selfDir + "/cleanroom"
	sibling := selfDir + "/cleanroom-guest-agent-linux-arm64"
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
		t.Fatalf("write prebuilt guest agent: %v", err)
	}
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write self binary: %v", err)
	}
	if err := os.WriteFile(sibling, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write sibling guest agent: %v", err)
	}

	got, err := discoverGuestAgentBinaryWith(
		"arm64",
		func(string) (string, error) { return "/usr/local/bin/cleanroom-guest-agent-linux-arm64", nil },
		func() (string, error) { return self, nil },
		func() (string, error) { return cwd, nil },
		os.Stat,
		func(path string) (bool, error) { return path == sibling, nil },
	)
	if err != nil {
		t.Fatalf("discoverGuestAgentBinaryWith returned error: %v", err)
	}
	if got != sibling {
		t.Fatalf("unexpected guest agent path: got %q want %q", got, sibling)
	}
}
