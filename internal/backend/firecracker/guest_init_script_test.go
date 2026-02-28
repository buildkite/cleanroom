package firecracker

import (
	"strings"
	"testing"
)

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
