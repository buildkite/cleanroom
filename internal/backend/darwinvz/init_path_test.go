//go:build darwin

package darwinvz

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/hosttools"
)

func TestGuestInitExecutableForRootFSUsesGuestAgentInit(t *testing.T) {
	t.Parallel()

	path, notice := guestInitExecutableForRootFS("/tmp/rootfs.ext4")
	if got, want := path, "/usr/local/bin/cleanroom-guest-agent"; got != want {
		t.Fatalf("unexpected init path: got %q want %q", got, want)
	}
	if notice != "" {
		t.Fatalf("expected empty notice, got %q", notice)
	}
}

func TestValidatePreparedRuntimeRootFSRequiresGuestAgent(t *testing.T) {
	t.Parallel()

	err := validatePreparedRuntimeRootFS(filepath.Join(t.TempDir(), "missing.ext4"))
	if err == nil {
		t.Fatal("expected validation error for missing guest agent")
	}
	if !strings.Contains(err.Error(), guestAgentPath) {
		t.Fatalf("expected guest agent path in validation error, got %v", err)
	}
}

func TestPreparedRuntimeRootFSRequiredPathsDoNotRequireBinSh(t *testing.T) {
	t.Parallel()

	for _, path := range preparedRuntimeRootFSRequiredPaths {
		if path == "/bin/sh" {
			t.Fatal("shell-less rootfs should not be rejected at prepare time")
		}
	}
}

func TestValidateRootFSInspectableRejectsUnreadableImage(t *testing.T) {
	t.Parallel()

	if _, err := hosttools.ResolveE2FSProgsBinary("debugfs"); err != nil {
		t.Skipf("debugfs unavailable: %v", err)
	}

	imagePath := filepath.Join(t.TempDir(), "not-ext4.img")
	if err := os.WriteFile(imagePath, []byte("not an ext4 image"), 0o600); err != nil {
		t.Fatalf("write invalid image: %v", err)
	}

	err := validateRootFSInspectable(imagePath)
	if err == nil {
		t.Fatal("expected invalid image inspection to fail")
	}
	if !strings.Contains(err.Error(), "inspect ext4 rootfs") {
		t.Fatalf("unexpected error: %v", err)
	}
}
