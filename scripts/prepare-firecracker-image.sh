#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<USAGE
Prepare a Firecracker rootfs image for Cleanroom launched execution.

This script will:
1. copy a prebuilt cleanroom-guest-agent into the guest rootfs at /usr/local/bin/cleanroom-guest-agent
2. install a tiny init at /sbin/cleanroom-init that starts the guest agent
3. install `strace` in the guest image for debugging

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
export CLEANROOM_VSOCK_PORT=$AGENT_PORT
while true; do
  /usr/local/bin/cleanroom-guest-agent || true
  sleep 1
done
INIT
chmod 0755 "$INIT_PATH"

if [[ "$INSTALL_MISE" -eq 1 ]]; then
  echo "installing mise + strace in guest image via apk"
  rm -f "$MOUNT_DIR/root/.local/bin/mise"
  chroot "$MOUNT_DIR" /bin/sh -lc "apk update && apk add --no-cache mise strace"

  MISE_CFG_DIR="$MOUNT_DIR/root/.config/mise"
  mkdir -p "$MISE_CFG_DIR"
  cat > "$MISE_CFG_DIR/config.toml" <<'MISECFG'
auto_install = false
exec_auto_install = false
not_found_auto_install = false
disable_default_registry = true
MISECFG
else
  echo "installing strace in guest image via apk"
  chroot "$MOUNT_DIR" /bin/sh -lc "apk update && apk add --no-cache strace"
fi

echo "rootfs prepared successfully"
echo "- rootfs image: $ROOTFS_IMAGE"
echo "- mount dir: $MOUNT_DIR"
echo "- agent binary: /usr/local/bin/cleanroom-guest-agent"
echo "- tiny init: /sbin/cleanroom-init"
echo "- agent port: $AGENT_PORT"
if [[ "$INSTALL_MISE" -eq 1 ]]; then
  echo "- installed guest packages: mise, strace"
  echo "- mise config: /root/.config/mise/config.toml"
else
  echo "- installed guest package: strace"
fi
