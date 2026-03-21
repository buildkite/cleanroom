package firecracker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyCompatibleGuestAgentInstallSourceUsesLegacyDistCopy(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	staged := filepath.Join(repoRoot, "dist", "linux-amd64", "libexec", "cleanroom", "cleanroom-guest-agent-linux-amd64")
	legacy := filepath.Join(repoRoot, "dist", "cleanroom-guest-agent-linux-amd64")
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		t.Fatalf("mkdir staged guest agent dir: %v", err)
	}
	if err := os.WriteFile(staged, []byte("staged"), 0o755); err != nil {
		t.Fatalf("write staged guest agent: %v", err)
	}
	if err := os.WriteFile(legacy, []byte("legacy"), 0o755); err != nil {
		t.Fatalf("write legacy guest agent: %v", err)
	}

	got := legacyCompatibleGuestAgentInstallSource(staged, os.Stat)
	if got != legacy {
		t.Fatalf("unexpected guest agent install source: got %q want %q", got, legacy)
	}
}

func TestLegacyCompatibleGuestAgentInstallSourceKeepsStagedPathWithoutCompatibilityCopy(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	staged := filepath.Join(repoRoot, "dist", "linux-amd64", "libexec", "cleanroom", "cleanroom-guest-agent-linux-amd64")
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		t.Fatalf("mkdir staged guest agent dir: %v", err)
	}
	if err := os.WriteFile(staged, []byte("staged"), 0o755); err != nil {
		t.Fatalf("write staged guest agent: %v", err)
	}

	got := legacyCompatibleGuestAgentInstallSource(staged, os.Stat)
	if got != staged {
		t.Fatalf("unexpected guest agent install source: got %q want %q", got, staged)
	}
}

func TestLegacyCompatibleGuestAgentInstallSourceKeepsInstalledLibexecPath(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "prefix", "libexec", "cleanroom", "cleanroom-guest-agent-linux-amd64")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir installed guest agent dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("installed"), 0o755); err != nil {
		t.Fatalf("write installed guest agent: %v", err)
	}

	got := legacyCompatibleGuestAgentInstallSource(path, os.Stat)
	if got != path {
		t.Fatalf("unexpected guest agent install source: got %q want %q", got, path)
	}
}
