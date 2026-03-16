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
	if !strings.Contains(guestInitScriptTemplate, "cleanroom_vmnet_guest_ipv4") {
		t.Fatal("expected vmnet static guest IPv4 boot arg lookup in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "cleanroom_vmnet_gateway_ipv4") {
		t.Fatal("expected vmnet static gateway boot arg lookup in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "cleanroom_vmnet_prefix_len") {
		t.Fatal("expected vmnet static prefix length boot arg lookup in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "ip addr add \"$VMNET_GUEST_IPV4/$VMNET_PREFIX_LEN\" dev \"$NET_IFACE\"") {
		t.Fatal("expected vmnet static address configuration in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "ip route replace default via \"$VMNET_GATEWAY_IPV4\" dev \"$NET_IFACE\"") {
		t.Fatal("expected vmnet default route configuration in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "printf 'nameserver %s\\n' \"$VMNET_GATEWAY_IPV4\" >/etc/resolv.conf") {
		t.Fatal("expected vmnet dns configuration in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "udhcpc -q -n -t 3 -T 3 -i") {
		t.Fatal("expected udhcpc DHCP fallback in init script")
	}
}

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
