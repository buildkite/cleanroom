//go:build darwin

package darwinvz

import (
	"strings"
	"testing"
)

func TestGuestInitScriptAlwaysStartsStdioAgentWhenSerialDeviceExists(t *testing.T) {
	if strings.Contains(guestInitScriptTemplate, "CLEANROOM_USE_STDIO") {
		t.Fatal("expected stdio transport to no longer be gated by CLEANROOM_USE_STDIO")
	}

	if !strings.Contains(guestInitScriptTemplate, "CLEANROOM_GUEST_TRANSPORT=stdio /usr/local/bin/cleanroom-guest-agent") {
		t.Fatal("expected stdio guest-agent launch in init script")
	}

	if !strings.Contains(guestInitScriptTemplate, ") &") {
		t.Fatal("expected stdio guest-agent loop to run in background")
	}
}

func TestGuestInitScriptBootstrapsNetwork(t *testing.T) {
	if !strings.Contains(guestInitScriptTemplate, "setup_guest_network") {
		t.Fatal("expected guest network setup function in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "udhcpc -q -n -t 3 -T 3 -i") {
		t.Fatal("expected udhcpc DHCP bootstrap in init script")
	}
}

func TestGuestInitScriptAutostartsDockerWhenAvailable(t *testing.T) {
	if !strings.Contains(guestInitScriptTemplate, "command -v dockerd") {
		t.Fatal("expected dockerd availability check in init script")
	}

	if !strings.Contains(guestInitScriptTemplate, "dockerd --host=unix:///var/run/docker.sock --iptables=false --storage-driver=vfs") {
		t.Fatal("expected dockerd to launch with unix socket in init script")
	}

	if !strings.Contains(guestInitScriptTemplate, "docker version >/dev/null 2>&1") {
		t.Fatal("expected init script to wait for dockerd API readiness")
	}
}
