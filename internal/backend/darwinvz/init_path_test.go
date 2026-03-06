//go:build darwin

package darwinvz

import "testing"

func TestGuestInitExecutableForShellPresenceUsesInitScriptWhenShellExists(t *testing.T) {
	t.Parallel()

	path, notice := guestInitExecutableForShellPresence(true)
	if got, want := path, "/sbin/cleanroom-init"; got != want {
		t.Fatalf("unexpected init path: got %q want %q", got, want)
	}
	if notice != "" {
		t.Fatalf("expected empty notice for shell-enabled rootfs, got %q", notice)
	}
}

func TestGuestInitExecutableForShellPresenceFallsBackToGuestAgentWhenShellMissing(t *testing.T) {
	t.Parallel()

	path, notice := guestInitExecutableForShellPresence(false)
	if got, want := path, "/usr/local/bin/cleanroom-guest-agent"; got != want {
		t.Fatalf("unexpected fallback init path: got %q want %q", got, want)
	}
	if notice == "" {
		t.Fatal("expected shell-less fallback notice")
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
