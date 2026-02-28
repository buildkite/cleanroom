#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<USAGE
Prepare a Firecracker rootfs image for Cleanroom launched execution.

This script will:
1. copy a prebuilt cleanroom-guest-agent into the guest rootfs at /usr/local/bin/cleanroom-guest-agent
2. install a tiny init at /sbin/cleanroom-init that starts the guest agent
3. install `git` and `strace` in the guest image

Usage:
  scripts/prepare-firecracker-image.sh \
    [--rootfs-image /path/to/rootfs.ext4] \
    [--mount-dir /mnt/rootfs] \
    [--agent-port 10700] \
    [--agent-binary /path/to/cleanroom-guest-agent] \
    [--install-mise]

Defaults:
- --rootfs-image: \${XDG_DATA_HOME:-~/.local/share}/cleanroom/images/rootfs.ext4
- --mount-dir: \${XDG_RUNTIME_DIR:-/tmp}/cleanroom/mnt/rootfs

Notes:
- If --mount-dir is already mounted to the rootfs, script uses it as-is.
- If not mounted, this script can loop-mount/unmount automatically only when run as root.
- If --rootfs-image points to a root-owned path (for example /root/...), the script will
  try to copy it into the user's XDG image path via sudo automatically.
- --install-mise installs Alpine package `mise` inside the guest image and
  writes a minimal `/root/.config/mise/config.toml`.
- `git` is installed by default for clone benchmarks and repo operations.
- `strace` is installed by default to support in-VM debugging.
USAGE
}

resolve_default_data_home() {
  if [[ -n "${XDG_DATA_HOME:-}" ]]; then
    echo "$XDG_DATA_HOME"
    return
  fi
  if [[ "$(id -u)" -eq 0 && -n "${SUDO_USER:-}" ]]; then
    local sudo_home
    sudo_home="$(getent passwd "$SUDO_USER" | cut -d: -f6)"
    if [[ -n "$sudo_home" ]]; then
      echo "$sudo_home/.local/share"
      return
    fi
  fi
  echo "$HOME/.local/share"
}

XDG_DATA_HOME_DEFAULT="$(resolve_default_data_home)"
XDG_RUNTIME_BASE="${XDG_RUNTIME_DIR:-/tmp}"
ROOTFS_IMAGE="$XDG_DATA_HOME_DEFAULT/cleanroom/images/rootfs.ext4"
USER_DEFAULT_ROOTFS_IMAGE="$ROOTFS_IMAGE"
MOUNT_DIR="$XDG_RUNTIME_BASE/cleanroom/mnt/rootfs"
AGENT_PORT="10700"
AGENT_BINARY=""
INSTALL_MISE=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --rootfs-image)
      ROOTFS_IMAGE="${2:-}"
      shift 2
      ;;
    --mount-dir)
      MOUNT_DIR="${2:-}"
      shift 2
      ;;
    --agent-port)
      AGENT_PORT="${2:-}"
      shift 2
      ;;
    --agent-binary)
      AGENT_BINARY="${2:-}"
      shift 2
      ;;
    --install-mise)
      INSTALL_MISE=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
done

ensure_user_copy_from_sudo() {
  local src="$1"
  local dst="$2"

  if [[ "$(id -u)" -eq 0 ]]; then
    return 1
  fi
  if ! command -v sudo >/dev/null 2>&1; then
    return 1
  fi
  if ! sudo test -f "$src" >/dev/null 2>&1; then
    return 1
  fi

  echo "copying root-owned image to user path:"
  echo "- source: $src"
  echo "- dest:   $dst"
  mkdir -p "$(dirname "$dst")"
  sudo cp "$src" "$dst"
  sudo chown "$(id -u):$(id -g)" "$dst"
  return 0
}

if [[ ! -f "$ROOTFS_IMAGE" ]]; then
  if ensure_user_copy_from_sudo "$ROOTFS_IMAGE" "$USER_DEFAULT_ROOTFS_IMAGE"; then
    ROOTFS_IMAGE="$USER_DEFAULT_ROOTFS_IMAGE"
  else
    echo "rootfs image not found: $ROOTFS_IMAGE" >&2
    echo "set --rootfs-image or place it at the default XDG path above" >&2
    exit 1
  fi
fi

if ! [[ "$AGENT_PORT" =~ ^[0-9]+$ ]] || (( AGENT_PORT < 1 || AGENT_PORT > 65535 )); then
  echo "invalid --agent-port: $AGENT_PORT" >&2
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ -z "$AGENT_BINARY" ]]; then
  AGENT_BINARY="$REPO_ROOT/dist/cleanroom-guest-agent"
fi

if [[ ! -x "$AGENT_BINARY" ]]; then
  echo "agent binary not found or not executable: $AGENT_BINARY" >&2
  echo "build it first (recommended: mise run build)" >&2
  exit 1
fi

mkdir -p "$MOUNT_DIR"

MOUNTED_BY_SCRIPT=0
if mountpoint -q "$MOUNT_DIR"; then
  echo "using existing mount: $MOUNT_DIR"
else
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "$MOUNT_DIR is not mounted and automatic mount requires root." >&2
    echo "Either mount it first, or run this script as root." >&2
    exit 1
  fi

  echo "mounting rootfs image at $MOUNT_DIR"
  mount -o loop "$ROOTFS_IMAGE" "$MOUNT_DIR"
  MOUNTED_BY_SCRIPT=1
fi

cleanup() {
  if [[ "$MOUNTED_BY_SCRIPT" -eq 1 ]]; then
    echo "unmounting $MOUNT_DIR"
    umount "$MOUNT_DIR"
  fi
}
trap cleanup EXIT

install -D -m 0755 "$AGENT_BINARY" "$MOUNT_DIR/usr/local/bin/cleanroom-guest-agent"

INIT_PATH="$MOUNT_DIR/sbin/cleanroom-init"
mkdir -p "$(dirname "$INIT_PATH")"
cat > "$INIT_PATH" <<INIT
#!/bin/sh
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

cmdline="\$(cat /proc/cmdline 2>/dev/null || true)"
arg_value() {
  key="\$1"
  for token in \$cmdline; do
    case "\$token" in
      "\$key"=*) echo "\${token#*=}"; return 0 ;;
    esac
  done
  return 1
}

GUEST_IP="\$(arg_value cleanroom_guest_ip || true)"
GUEST_GW="\$(arg_value cleanroom_guest_gw || true)"
GUEST_MASK="\$(arg_value cleanroom_guest_mask || true)"
GUEST_DNS="\$(arg_value cleanroom_guest_dns || true)"

if command -v ip >/dev/null 2>&1 && [ -n "\$GUEST_IP" ]; then
  [ -n "\$GUEST_MASK" ] || GUEST_MASK="24"
  ip link set dev eth0 up 2>/dev/null || true
  ip addr flush dev eth0 2>/dev/null || true
  ip addr add "\$GUEST_IP/\$GUEST_MASK" dev eth0 2>/dev/null || true
  if [ -n "\$GUEST_GW" ]; then
    ip route add default via "\$GUEST_GW" dev eth0 2>/dev/null || true
  fi
  if [ -n "\$GUEST_DNS" ]; then
    printf 'nameserver %s\n' "\$GUEST_DNS" > /etc/resolv.conf 2>/dev/null || true
  fi
fi

export CLEANROOM_VSOCK_PORT=$AGENT_PORT
DOCKER_REQUIRED="\$(arg_value cleanroom_service_docker_required || true)"
if [ "\$DOCKER_REQUIRED" = "1" ] && command -v dockerd >/dev/null 2>&1; then
  DOCKER_STARTUP_TIMEOUT="\$(arg_value cleanroom_service_docker_startup_timeout || true)"
  case "\$DOCKER_STARTUP_TIMEOUT" in
    ''|*[!0-9]*) DOCKER_STARTUP_TIMEOUT="20" ;;
  esac
  if [ "\$DOCKER_STARTUP_TIMEOUT" -le 0 ]; then
    DOCKER_STARTUP_TIMEOUT="20"
  fi
  DOCKER_STORAGE_DRIVER="\$(arg_value cleanroom_service_docker_storage_driver || true)"
  if [ -z "\$DOCKER_STORAGE_DRIVER" ]; then
    DOCKER_STORAGE_DRIVER="vfs"
  fi
  DOCKER_IPTABLES="\$(arg_value cleanroom_service_docker_iptables || true)"

  DOCKER_ARGS="--host=unix:///var/run/docker.sock --storage-driver=\$DOCKER_STORAGE_DRIVER"
  if [ "\$DOCKER_IPTABLES" = "0" ] || [ "\$DOCKER_IPTABLES" = "false" ]; then
    DOCKER_ARGS="\$DOCKER_ARGS --iptables=false"
  fi

  mkdir -p /var/log /var/lib/docker /etc/docker /var/run /sys/fs/cgroup
  mount -t cgroup2 none /sys/fs/cgroup 2>/dev/null || true
  if [ ! -S /var/run/docker.sock ]; then
    dockerd \$DOCKER_ARGS >/var/log/dockerd.log 2>&1 &
  fi
  i=0
  DOCKER_WAIT_TICKS=\$((DOCKER_STARTUP_TIMEOUT * 10))
  while [ "\$i" -lt "\$DOCKER_WAIT_TICKS" ]; do
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
    i=\$((i + 1))
  done
fi
while true; do
  /usr/local/bin/cleanroom-guest-agent || true
  sleep 1
done
INIT
chmod 0755 "$INIT_PATH"

if [[ "$INSTALL_MISE" -eq 1 ]]; then
  echo "installing mise + git + strace in guest image via apk"
  rm -f "$MOUNT_DIR/root/.local/bin/mise"
  chroot "$MOUNT_DIR" /bin/sh -lc "apk update && apk add --no-cache mise git strace"

  MISE_CFG_DIR="$MOUNT_DIR/root/.config/mise"
  mkdir -p "$MISE_CFG_DIR"
  cat > "$MISE_CFG_DIR/config.toml" <<'MISECFG'
auto_install = false
exec_auto_install = false
not_found_auto_install = false
disable_default_registry = true
MISECFG
else
  echo "installing git + strace in guest image via apk"
  chroot "$MOUNT_DIR" /bin/sh -lc "apk update && apk add --no-cache git strace"
fi

echo "rootfs prepared successfully"
echo "- rootfs image: $ROOTFS_IMAGE"
echo "- mount dir: $MOUNT_DIR"
echo "- agent binary: /usr/local/bin/cleanroom-guest-agent"
echo "- tiny init: /sbin/cleanroom-init"
echo "- agent port: $AGENT_PORT"
if [[ "$INSTALL_MISE" -eq 1 ]]; then
  echo "- installed guest packages: mise, git, strace"
  echo "- mise config: /root/.config/mise/config.toml"
else
  echo "- installed guest packages: git, strace"
fi
