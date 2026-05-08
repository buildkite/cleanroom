package firecracker

import (
	"strings"
	"testing"
)

func TestGuestInitScriptDoesNotStartDocker(t *testing.T) {
	noDockerStrings := []string{
		"dockerd",
		"cleanroom_service_docker_required",
		"DOCKER_REQUIRED",
		"/var/lib/docker",
		"docker version",
	}
	for _, s := range noDockerStrings {
		if strings.Contains(guestInitScriptTemplate, s) {
			t.Fatalf("expected init script not to contain %q (docker lifecycle moved to agent)", s)
		}
	}
}

func TestGuestInitScriptAddsLocalhostHostsEntries(t *testing.T) {
	if !strings.Contains(guestInitScriptTemplate, "127.0.0.1 localhost") {
		t.Fatal("expected localhost IPv4 hosts entry in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "::1 localhost ip6-localhost ip6-loopback") {
		t.Fatal("expected localhost IPv6 hosts entry in init script")
	}
	if strings.Contains(guestInitScriptTemplate, "cat >/etc/hosts") {
		t.Fatal("expected init script to preserve existing hosts entries")
	}
	if !strings.Contains(guestInitScriptTemplate, "append_hosts_line_if_missing()") {
		t.Fatal("expected init script to use helper for missing localhost hosts entries")
	}
	if !strings.Contains(guestInitScriptTemplate, "tail -c 1 /etc/hosts") {
		t.Fatal("expected localhost hosts entry helper to detect missing trailing newline")
	}
	if !strings.Contains(guestInitScriptTemplate, "printf '\\n' >>/etc/hosts 2>/dev/null || true") {
		t.Fatal("expected localhost hosts entry helper to insert a separator newline when needed")
	}
	if !strings.Contains(guestInitScriptTemplate, "printf '%s\\n' \"$line\" >>/etc/hosts 2>/dev/null || true") {
		t.Fatal("expected localhost hosts entry helper to append lines best-effort")
	}
}

func TestGuestInitScriptConfiguresLoopback(t *testing.T) {
	if !strings.Contains(guestInitScriptTemplate, "ip link set dev lo up") {
		t.Fatal("expected init script to bring up loopback")
	}
	if !strings.Contains(guestInitScriptTemplate, "ip addr add 127.0.0.1/8 dev lo") {
		t.Fatal("expected init script to configure IPv4 loopback")
	}
}
