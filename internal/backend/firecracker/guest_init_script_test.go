package firecracker

import (
	"strings"
	"testing"
)

func TestGuestInitScriptAutostartsDockerWhenAvailable(t *testing.T) {
	if !strings.Contains(guestInitScriptTemplate, "DOCKER_REQUIRED=\"$(arg_value cleanroom_service_docker_required || true)\"") {
		t.Fatal("expected docker service required flag lookup in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "[ \"$DOCKER_REQUIRED\" = \"1\" ] && command -v dockerd >/dev/null 2>&1") {
		t.Fatal("expected dockerd launch to be gated by docker service contract")
	}
	if !strings.Contains(guestInitScriptTemplate, "DOCKER_STORAGE_DRIVER=\"$(arg_value cleanroom_service_docker_storage_driver || true)\"") {
		t.Fatal("expected docker storage driver boot arg lookup in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "DOCKER_IPTABLES=\"$(arg_value cleanroom_service_docker_iptables || true)\"") {
		t.Fatal("expected docker iptables boot arg lookup in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "docker version >/dev/null 2>&1") {
		t.Fatal("expected init script to wait for dockerd API readiness")
	}
}

func TestGuestInitScriptSeedsLocalhostHostsEntries(t *testing.T) {
	if !strings.Contains(guestInitScriptTemplate, "127.0.0.1 localhost") {
		t.Fatal("expected localhost IPv4 hosts entry in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "::1 localhost ip6-localhost ip6-loopback") {
		t.Fatal("expected localhost IPv6 hosts entry in init script")
	}
	if strings.Contains(guestInitScriptTemplate, "cat >/etc/hosts") {
		t.Fatal("expected init script to preserve existing hosts entries")
	}
	if !strings.Contains(guestInitScriptTemplate, ">>/etc/hosts") {
		t.Fatal("expected init script to append missing localhost hosts entries")
	}
	if !strings.Contains(guestInitScriptTemplate, ">>/etc/hosts 2>/dev/null || true") {
		t.Fatal("expected localhost hosts entry append to be best-effort")
	}
}
