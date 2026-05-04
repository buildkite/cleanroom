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
	if !strings.Contains(guestInitScriptTemplate, "DOCKER_MIRROR_HOST=\"$(arg_value cleanroom_service_docker_registry_mirror_host || true)\"") {
		t.Fatal("expected docker registry mirror host boot arg lookup in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "DOCKER_MIRROR_PORT=\"$(arg_value cleanroom_service_docker_registry_mirror_port || true)\"") {
		t.Fatal("expected docker registry mirror port boot arg lookup in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "DOCKER_MIRROR_REGISTRIES=\"$(arg_value cleanroom_service_docker_registry_mirror_registries || true)\"") {
		t.Fatal("expected docker registry mirror registries boot arg lookup in init script")
	}
	if !strings.Contains(guestInitScriptTemplate, "--registry-mirror=http://$DOCKER_MIRROR_HOST:$DOCKER_MIRROR_PORT") {
		t.Fatal("expected init script to configure dockerd registry mirror when provided")
	}
	if !strings.Contains(guestInitScriptTemplate, "--insecure-registry=$DOCKER_MIRROR_HOST:$DOCKER_MIRROR_PORT") {
		t.Fatal("expected init script to mark the dockerd mirror as insecure for guest HTTP access")
	}
	if !strings.Contains(guestInitScriptTemplate, `mirror_dir="/etc/docker/certs.d/$registry"`) {
		t.Fatal("expected init script to configure per-registry Docker host config")
	}
	if strings.Contains(guestInitScriptTemplate, `/etc/containerd/certs.d`) {
		t.Fatal("expected init script to write registry host config where dockerd reads it")
	}
	if !strings.Contains(guestInitScriptTemplate, `printf 'server = "http://%s:%s/registry/%s"\n' "$DOCKER_MIRROR_HOST" "$DOCKER_MIRROR_PORT" "$registry"`) {
		t.Fatal("expected init script to make the Cleanroom registry gateway path authoritative")
	}
	if strings.Contains(guestInitScriptTemplate, `printf 'server = "https://%s"\n' "$registry"`) {
		t.Fatal("expected init script not to configure direct upstream fallback")
	}

	if !strings.Contains(guestInitScriptTemplate, "docker version >/dev/null 2>&1") {
		t.Fatal("expected init script to wait for dockerd API readiness")
	}
}
