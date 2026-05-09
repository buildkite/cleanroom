//go:build darwin

package darwinvz

import (
	"os/exec"
	"strings"
	"testing"
)

func TestGuestInitScriptSyntax(t *testing.T) {
	t.Parallel()

	if out, err := exec.Command("sh", "-n", "-c", guestInitScriptTemplate).CombinedOutput(); err != nil {
		t.Fatalf("guest init script syntax check failed: %v\n%s", err, out)
	}
}

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
	if !strings.Contains(guestInitScriptTemplate, "CLEANROOM_GUEST_BOOT_TIMINGS") {
		t.Fatal("expected init script to export guest boot timings")
	}
	if !strings.Contains(guestInitScriptTemplate, "mark_guest_boot_timing()") {
		t.Fatal("expected init script to define guest boot timing helper")
	}
	if !strings.Contains(guestInitScriptTemplate, "cleanroom_guest_boot_timing=1") {
		t.Fatal("expected init script to gate boot timing on kernel cmdline")
	}
	if !strings.Contains(guestInitScriptTemplate, "guest_init_vsock_agent_exec") {
		t.Fatal("expected init script to mark the vsock guest-agent handoff")
	}
	for _, marker := range []string{
		"guest_init_core_mounts_done",
		"guest_init_hosts_done",
		"guest_init_network_done",
		"guest_init_stdio_agent_exec",
	} {
		if strings.Contains(guestInitScriptTemplate, marker) {
			t.Fatalf("expected init script not to mark %s in the hot path", marker)
		}
	}
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
	if !strings.Contains(guestInitScriptTemplate, "ip link set lo up") {
		t.Fatal("expected loopback interface setup in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "ip addr add 127.0.0.1/8 dev lo") {
		t.Fatal("expected IPv4 loopback address setup in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "ip -6 addr add ::1/128 dev lo") {
		t.Fatal("expected IPv6 loopback address setup in init script")
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

func TestGuestInitScriptConfiguresLoopback(t *testing.T) {
	if !strings.Contains(guestInitScriptTemplate, "ip link set lo up") {
		t.Fatal("expected init script to bring up loopback")
	}
	if !strings.Contains(guestInitScriptTemplate, "ip addr add 127.0.0.1/8 dev lo") {
		t.Fatal("expected init script to configure IPv4 loopback")
	}
}

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
