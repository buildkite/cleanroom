//go:build darwin

package darwinvz

import (
	"fmt"
	"os"
	"strings"

	"github.com/buildkite/cleanroom/internal/hosttools"
)

const (
	guestInitScriptPathSbin    = "/sbin/cleanroom-init"
	guestInitScriptPathUsrSbin = "/usr/sbin/cleanroom-init"
	guestAgentPath             = "/usr/local/bin/cleanroom-guest-agent"
)

const guestInitScriptTemplate = `#!/bin/sh
set -eu

mount -t proc proc /proc 2>/dev/null || true
mount -t sysfs sysfs /sys 2>/dev/null || true
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
mkdir -p /dev/pts /run /tmp
mount -t devpts devpts /dev/pts 2>/dev/null || true
mount -t tmpfs tmpfs /run 2>/dev/null || true
mount -t tmpfs tmpfs /tmp 2>/dev/null || true

export HOME=/root
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/root/.local/bin

mkdir -p /etc 2>/dev/null || true
touch /etc/hosts 2>/dev/null || true
append_hosts_line_if_missing() {
  pattern="$1"
  line="$2"
  if grep -Eq "$pattern" /etc/hosts 2>/dev/null; then
    return 0
  fi
  if [ -s /etc/hosts ] && [ -n "$(tail -c 1 /etc/hosts 2>/dev/null || true)" ]; then
    printf '\n' >>/etc/hosts 2>/dev/null || true
  fi
  printf '%s\n' "$line" >>/etc/hosts 2>/dev/null || true
}
append_hosts_line_if_missing '^[[:space:]]*127\.0\.0\.1([[:space:]]|$).*localhost([[:space:]]|$)' '127.0.0.1 localhost'
append_hosts_line_if_missing '^[[:space:]]*::1([[:space:]]|$).*localhost([[:space:]]|$)' '::1 localhost ip6-localhost ip6-loopback'

cmdline="$(cat /proc/cmdline 2>/dev/null || true)"
arg_value() {
  key="$1"
  for token in $cmdline; do
    case "$token" in
      "$key"=*) echo "${token#*=}"; return 0 ;;
    esac
  done
  return 1
}

prefix_to_mask() {
  prefix="$1"
  remaining="$prefix"
  octet_index=0
  mask=""
  while [ "$octet_index" -lt 4 ]; do
    if [ "$remaining" -ge 8 ]; then
      octet=255
      remaining=$((remaining - 8))
    elif [ "$remaining" -gt 0 ]; then
      octet=$((256 - (1 << (8 - remaining))))
      remaining=0
    else
      octet=0
    fi
    if [ -z "$mask" ]; then
      mask="$octet"
    else
      mask="$mask.$octet"
    fi
    octet_index=$((octet_index + 1))
  done
  printf '%s\n' "$mask"
}

configure_vmnet_static_network() {
  NET_IFACE="$1"
  VMNET_GUEST_IPV4="$(arg_value cleanroom_vmnet_guest_ipv4 || true)"
  VMNET_GATEWAY_IPV4="$(arg_value cleanroom_vmnet_gateway_ipv4 || true)"
  VMNET_PREFIX_LEN="$(arg_value cleanroom_vmnet_prefix_len || true)"

  if [ -z "$VMNET_GUEST_IPV4" ] || [ -z "$VMNET_GATEWAY_IPV4" ] || [ -z "$VMNET_PREFIX_LEN" ]; then
    return 1
  fi

  if command -v ip >/dev/null 2>&1; then
    ip link set "$NET_IFACE" up 2>/dev/null || true
    ip addr flush dev "$NET_IFACE" scope global 2>/dev/null || true
    ip addr add "$VMNET_GUEST_IPV4/$VMNET_PREFIX_LEN" dev "$NET_IFACE"
    ip route replace default via "$VMNET_GATEWAY_IPV4" dev "$NET_IFACE"
  elif command -v ifconfig >/dev/null 2>&1; then
    VMNET_NETMASK="$(prefix_to_mask "$VMNET_PREFIX_LEN")"
    ifconfig "$NET_IFACE" "$VMNET_GUEST_IPV4" netmask "$VMNET_NETMASK" up
    if command -v route >/dev/null 2>&1; then
      route del default >/dev/null 2>&1 || true
      route add default gw "$VMNET_GATEWAY_IPV4" >/dev/null 2>&1 || true
    fi
  else
    return 1
  fi

  mkdir -p /etc 2>/dev/null || true
  printf 'nameserver %s\n' "$VMNET_GATEWAY_IPV4" >/etc/resolv.conf 2>/dev/null || true
  return 0
}

setup_loopback() {
  if command -v ip >/dev/null 2>&1; then
    ip link set lo up 2>/dev/null || true
    ip addr add 127.0.0.1/8 dev lo 2>/dev/null || true
    ip -6 addr add ::1/128 dev lo 2>/dev/null || true
  elif command -v ifconfig >/dev/null 2>&1; then
    ifconfig lo 127.0.0.1 up 2>/dev/null || true
    ifconfig lo inet6 add ::1/128 2>/dev/null || true
  fi
}

setup_guest_network() {
  setup_loopback

  NET_IFACE=""
  for cand in /sys/class/net/*; do
    name="$(basename "$cand")"
    if [ "$name" = "lo" ]; then
      continue
    fi
    NET_IFACE="$name"
    break
  done
  if [ -z "$NET_IFACE" ]; then
    return 0
  fi

  if configure_vmnet_static_network "$NET_IFACE"; then
    return 0
  fi

  if command -v ip >/dev/null 2>&1; then
    ip link set "$NET_IFACE" up 2>/dev/null || true
  elif command -v ifconfig >/dev/null 2>&1; then
    ifconfig "$NET_IFACE" up 2>/dev/null || true
  fi

  if command -v udhcpc >/dev/null 2>&1; then
    udhcpc -q -n -t 3 -T 3 -i "$NET_IFACE" >/dev/null 2>&1 || true
  elif command -v dhclient >/dev/null 2>&1; then
    dhclient -1 "$NET_IFACE" >/dev/null 2>&1 || true
  fi
}
setup_guest_network

GUEST_PORT="$(arg_value cleanroom_guest_port || true)"
if [ -z "$GUEST_PORT" ]; then
  GUEST_PORT="10700"
fi
export CLEANROOM_VSOCK_PORT="$GUEST_PORT"

DOCKER_REQUIRED="$(arg_value cleanroom_service_docker_required || true)"
if [ "$DOCKER_REQUIRED" = "1" ] && command -v dockerd >/dev/null 2>&1; then
  DOCKER_STARTUP_TIMEOUT="$(arg_value cleanroom_service_docker_startup_timeout || true)"
  case "$DOCKER_STARTUP_TIMEOUT" in
    ''|*[!0-9]*) DOCKER_STARTUP_TIMEOUT="20" ;;
  esac
  if [ "$DOCKER_STARTUP_TIMEOUT" -le 0 ]; then
    DOCKER_STARTUP_TIMEOUT="20"
  fi
  DOCKER_STORAGE_DRIVER="$(arg_value cleanroom_service_docker_storage_driver || true)"
  if [ -z "$DOCKER_STORAGE_DRIVER" ]; then
    DOCKER_STORAGE_DRIVER="overlay2"
  fi
  DOCKER_IPTABLES="$(arg_value cleanroom_service_docker_iptables || true)"
  DOCKER_MIRROR_HOST="$(arg_value cleanroom_service_docker_registry_mirror_host || true)"
  DOCKER_MIRROR_PORT="$(arg_value cleanroom_service_docker_registry_mirror_port || true)"
  DOCKER_MIRROR_REGISTRIES="$(arg_value cleanroom_service_docker_registry_mirror_registries || true)"
  case "$DOCKER_MIRROR_PORT" in
    ''|*[!0-9]*) DOCKER_MIRROR_PORT="" ;;
  esac

  if [ -n "$DOCKER_MIRROR_HOST" ] && [ -n "$DOCKER_MIRROR_PORT" ] && [ -n "$DOCKER_MIRROR_REGISTRIES" ]; then
    old_ifs="$IFS"
    IFS=','
    for registry in $DOCKER_MIRROR_REGISTRIES; do
      IFS="$old_ifs"
      case "$registry" in
        ''|*/*|*[[:space:]]*) ;;
        *)
          mirror_dir="/etc/docker/certs.d/$registry"
          mkdir -p "$mirror_dir" 2>/dev/null || true
          {
            printf 'server = "http://%s:%s/registry/%s"\n' "$DOCKER_MIRROR_HOST" "$DOCKER_MIRROR_PORT" "$registry"
          } > "$mirror_dir/hosts.toml" 2>/dev/null || true
        ;;
      esac
      IFS=','
    done
    IFS="$old_ifs"
  fi

  DOCKER_ARGS="--host=unix:///var/run/docker.sock --storage-driver=$DOCKER_STORAGE_DRIVER"
  if [ "$DOCKER_IPTABLES" = "0" ] || [ "$DOCKER_IPTABLES" = "false" ]; then
    DOCKER_ARGS="$DOCKER_ARGS --iptables=false"
  fi
  if [ -n "$DOCKER_MIRROR_HOST" ] && [ -n "$DOCKER_MIRROR_PORT" ]; then
    DOCKER_ARGS="$DOCKER_ARGS --registry-mirror=http://$DOCKER_MIRROR_HOST:$DOCKER_MIRROR_PORT"
    DOCKER_ARGS="$DOCKER_ARGS --insecure-registry=$DOCKER_MIRROR_HOST:$DOCKER_MIRROR_PORT"
  fi

  mkdir -p /var/log /var/lib/docker /etc/docker /var/run /sys/fs/cgroup
  mount -t cgroup2 none /sys/fs/cgroup 2>/dev/null || true
  if [ ! -S /var/run/docker.sock ]; then
    dockerd $DOCKER_ARGS >/var/log/dockerd.log 2>&1 &
  fi
  i=0
  DOCKER_WAIT_TICKS=$((DOCKER_STARTUP_TIMEOUT * 10))
  while [ "$i" -lt "$DOCKER_WAIT_TICKS" ]; do
    if [ -S /var/run/docker.sock ]; then
      if command -v docker >/dev/null 2>&1; then
        if docker version >/dev/null 2>&1; then
          break
        fi
      else
        break
      fi
    fi
    sleep 0.1
    i=$((i + 1))
  done
fi

AGENT_DEV=""
if [ -c /dev/hvc1 ]; then
  AGENT_DEV="/dev/hvc1"
elif [ -c /dev/vport1p0 ]; then
  AGENT_DEV="/dev/vport1p0"
fi

if [ -n "$AGENT_DEV" ]; then
  stty raw -echo <"$AGENT_DEV" 2>/dev/null || true
  (
    while true; do
      CLEANROOM_GUEST_TRANSPORT=stdio /usr/local/bin/cleanroom-guest-agent <"$AGENT_DEV" >"$AGENT_DEV" 2>/dev/hvc0 || true
      sleep 1
    done
  ) &
fi

while true; do
  /usr/local/bin/cleanroom-guest-agent || true
  sleep 1
done
`

type ext4PathKind string

const (
	ext4PathKindUnknown   ext4PathKind = ""
	ext4PathKindDirectory ext4PathKind = "directory"
	ext4PathKindRegular   ext4PathKind = "regular"
	ext4PathKindSymlink   ext4PathKind = "symlink"
)

func guestInitExecutableForRootFS(rootFSPath string) (path, notice string) {
	return guestInitExecutableForRootFSLayout(ext4PathExists(rootFSPath, "/bin/sh"), ext4PathType(rootFSPath, "/sbin"))
}

func guestInitExecutableForRootFSLayout(hasShell bool, sbinKind ext4PathKind) (path, notice string) {
	return guestInitExecutableForShellPresence(hasShell, preferredGuestInitScriptPathForSbinKind(sbinKind))
}

func guestInitExecutableForShellPresence(hasShell bool, initScriptPath string) (path, notice string) {
	if hasShell {
		if strings.TrimSpace(initScriptPath) == "" {
			initScriptPath = guestInitScriptPathUsrSbin
		}
		return initScriptPath, ""
	}
	return guestAgentPath, fmt.Sprintf("rootfs is shell-less; using %s as init", guestAgentPath)
}

func preferredGuestInitScriptPathForSbinKind(kind ext4PathKind) string {
	if kind == ext4PathKindDirectory {
		return guestInitScriptPathSbin
	}
	return guestInitScriptPathUsrSbin
}

func validatePreparedRuntimeRootFSInitPathForLayout(hasShell bool, sbinKind ext4PathKind, pathExists func(string) bool) error {
	initPath, _ := guestInitExecutableForRootFSLayout(hasShell, sbinKind)
	if initPath == guestAgentPath {
		return nil
	}
	if pathExists != nil && pathExists(initPath) {
		return nil
	}
	return fmt.Errorf("required runtime file %q is missing or unreadable", initPath)
}

func (a *Adapter) installGuestRuntimeIntoRootFS(rootFSPath, guestAgentBinaryPath string) error {
	if _, err := hosttools.ResolveE2FSProgsBinary("debugfs"); err != nil {
		return fmt.Errorf("find debugfs for runtime rootfs preparation: %w", err)
	}
	initScriptPath, err := createGuestInitScript()
	if err != nil {
		return err
	}
	defer os.Remove(initScriptPath)

	if err := injectFileIntoExt4(rootFSPath, guestAgentBinaryPath, guestAgentPath, 0o755); err != nil {
		return fmt.Errorf("inject guest agent into rootfs image: %w", err)
	}
	if err := injectFileIntoExt4(rootFSPath, initScriptPath, guestInitScriptPathUsrSbin, 0o755); err != nil {
		return fmt.Errorf("inject cleanroom init into rootfs image (%s): %w", guestInitScriptPathUsrSbin, err)
	}
	if ext4PathType(rootFSPath, "/sbin") == ext4PathKindDirectory {
		if err := injectFileIntoExt4(rootFSPath, initScriptPath, guestInitScriptPathSbin, 0o755); err != nil {
			return fmt.Errorf("inject cleanroom init into rootfs image (%s): %w", guestInitScriptPathSbin, err)
		}
	}
	return nil
}

func createGuestInitScript() (string, error) {
	f, err := os.CreateTemp("", "cleanroom-init-*.sh")
	if err != nil {
		return "", fmt.Errorf("create guest init script: %w", err)
	}
	if _, err := f.WriteString(guestInitScriptTemplate); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write guest init script: %w", err)
	}
	if err := f.Chmod(0o755); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("chmod guest init script: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("close guest init script: %w", err)
	}
	return f.Name(), nil
}
