//go:build darwin

package darwinvz

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/hosttools"
)

func TestGuestInitExecutableForShellPresenceUsesInitScriptWhenShellExists(t *testing.T) {
	t.Parallel()

	path, notice := guestInitExecutableForShellPresence(true, guestInitScriptPathUsrSbin)
	if got, want := path, "/usr/sbin/cleanroom-init"; got != want {
		t.Fatalf("unexpected init path: got %q want %q", got, want)
	}
	if notice != "" {
		t.Fatalf("expected empty notice for shell-enabled rootfs, got %q", notice)
	}
}

func TestGuestInitExecutableForShellPresenceFallsBackToGuestAgentWhenShellMissing(t *testing.T) {
	t.Parallel()

	path, notice := guestInitExecutableForShellPresence(false, guestInitScriptPathUsrSbin)
	if got, want := path, "/usr/local/bin/cleanroom-guest-agent"; got != want {
		t.Fatalf("unexpected fallback init path: got %q want %q", got, want)
	}
	if notice == "" {
		t.Fatal("expected shell-less fallback notice")
	}
}

func TestPreferredGuestInitScriptPathForSbinKindUsesSbinForRealDirectory(t *testing.T) {
	t.Parallel()

	if got, want := preferredGuestInitScriptPathForSbinKind(ext4PathKindDirectory), guestInitScriptPathSbin; got != want {
		t.Fatalf("unexpected init path: got %q want %q", got, want)
	}
}

func TestPreferredGuestInitScriptPathForSbinKindUsesUsrSbinForSymlinkLayout(t *testing.T) {
	t.Parallel()

	if got, want := preferredGuestInitScriptPathForSbinKind(ext4PathKindSymlink), guestInitScriptPathUsrSbin; got != want {
		t.Fatalf("unexpected init path: got %q want %q", got, want)
	}
}

func TestValidatePreparedRuntimeRootFSInitPathForLayoutRejectsStaleUsrSbinOnlyCache(t *testing.T) {
	t.Parallel()

	err := validatePreparedRuntimeRootFSInitPathForLayout(true, ext4PathKindDirectory, func(path string) bool {
		return path == guestInitScriptPathUsrSbin
	})
	if err == nil {
		t.Fatal("expected validation error for missing /sbin init path")
	}
	if !strings.Contains(err.Error(), guestInitScriptPathSbin) {
		t.Fatalf("expected missing /sbin init path error, got %v", err)
	}
}

func TestValidatePreparedRuntimeRootFSInitPathForLayoutAcceptsUsrSbinForSymlinkLayout(t *testing.T) {
	t.Parallel()

	err := validatePreparedRuntimeRootFSInitPathForLayout(true, ext4PathKindSymlink, func(path string) bool {
		return path == guestInitScriptPathUsrSbin
	})
	if err != nil {
		t.Fatalf("expected /usr/sbin init path to validate for symlink layout, got %v", err)
	}
}

func TestValidatePreparedRuntimeRootFSInitPathForLayoutAllowsShelllessRootFS(t *testing.T) {
	t.Parallel()

	err := validatePreparedRuntimeRootFSInitPathForLayout(false, ext4PathKindDirectory, func(path string) bool {
		return false
	})
	if err != nil {
		t.Fatalf("expected shell-less rootfs to rely on guest agent validation, got %v", err)
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
