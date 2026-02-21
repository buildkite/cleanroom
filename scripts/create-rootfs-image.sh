#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<USAGE
Create a Firecracker-compatible Alpine rootfs ext4 image for Cleanroom.

Usage:
  sudo scripts/create-rootfs-image.sh [options]

Options:
  --output PATH          Output rootfs image path.
                         Default: invoking user's
                         \${XDG_DATA_HOME:-~/.local/share}/cleanroom/images/rootfs.ext4
  --size-mb N            Image size in MiB (default: 1024)
  --arch NAME            Alpine arch (default: x86_64)
  --release NAME         Alpine release branch, e.g. v3.22 (default: latest-stable)
  --mirror URL           Alpine mirror base URL
                         (default: https://dl-cdn.alpinelinux.org/alpine)
  --tarball-url URL      Use explicit minirootfs tarball URL (overrides release lookup)
  -h, --help             Show this help

What this script does:
1. resolves an Alpine minirootfs tarball
2. unpacks it into a temporary rootfs
3. applies minimal VM defaults and installs base developer tooling
4. packs it into an ext4 disk image

Requirements:
- root privileges
- curl, tar, mkfs.ext4, mount, umount
USAGE
}

TARGET_USER="${SUDO_USER:-root}"
TARGET_UID="${SUDO_UID:-0}"
TARGET_GID="${SUDO_GID:-0}"
TARGET_HOME="$HOME"

if [[ -n "${SUDO_USER:-}" && "${SUDO_USER}" != "root" ]]; then
  TARGET_HOME="$(getent passwd "$SUDO_USER" | cut -d: -f6)"
fi

TARGET_XDG_DATA_HOME="${XDG_DATA_HOME:-$TARGET_HOME/.local/share}"
OUTPUT="$TARGET_XDG_DATA_HOME/cleanroom/images/rootfs.ext4"
SIZE_MB=1024
ARCH="x86_64"
RELEASE="latest-stable"
MIRROR="https://dl-cdn.alpinelinux.org/alpine"
TARBALL_URL=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --output)
      OUTPUT="${2:-}"
      shift 2
      ;;
    --size-mb)
      SIZE_MB="${2:-}"
      shift 2
      ;;
    --arch)
      ARCH="${2:-}"
      shift 2
      ;;
    --release)
      RELEASE="${2:-}"
      shift 2
      ;;
    --mirror)
      MIRROR="${2:-}"
      shift 2
      ;;
    --tarball-url)
      TARBALL_URL="${2:-}"
      shift 2
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

if [[ "$(id -u)" -ne 0 ]]; then
  echo "this script must run as root (use sudo)" >&2
  exit 1
fi

if ! [[ "$SIZE_MB" =~ ^[0-9]+$ ]] || (( SIZE_MB < 256 )); then
  echo "invalid --size-mb: $SIZE_MB (must be integer >= 256)" >&2
  exit 1
fi

for cmd in curl tar mkfs.ext4 mount umount; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "missing required command: $cmd" >&2
    exit 1
  fi
done

resolve_tarball_url() {
  if [[ -n "$TARBALL_URL" ]]; then
    echo "$TARBALL_URL"
    return 0
  fi

  local release_path
  if [[ "$RELEASE" == "latest-stable" ]]; then
    release_path="latest-stable"
  else
    release_path="$RELEASE"
  fi

  local index_url="$MIRROR/$release_path/releases/$ARCH/latest-releases.yaml"
  local filename
  filename="$(curl -fsSL "$index_url" | awk '/file: alpine-minirootfs-.*\.tar\.gz/{print $2; exit}')"
  if [[ -z "$filename" ]]; then
    echo "failed to resolve minirootfs filename from $index_url" >&2
    return 1
  fi

  echo "$MIRROR/$release_path/releases/$ARCH/$filename"
}

mkdir -p "$(dirname "$OUTPUT")"
TMP_DIR="$(mktemp -d /tmp/cleanroom-rootfs-build.XXXXXX)"
ROOTFS_DIR="$TMP_DIR/rootfs"
MOUNT_DIR="$TMP_DIR/mnt"
TARBALL_PATH="$TMP_DIR/minirootfs.tar.gz"

cleanup() {
  if mountpoint -q "$MOUNT_DIR"; then
    umount "$MOUNT_DIR" || true
  fi
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

TARBALL_URL="$(resolve_tarball_url)"
echo "downloading Alpine minirootfs: $TARBALL_URL"
curl -fL "$TARBALL_URL" -o "$TARBALL_PATH"

mkdir -p "$ROOTFS_DIR" "$MOUNT_DIR"
tar -xzf "$TARBALL_PATH" -C "$ROOTFS_DIR"

# Basic defaults for local microVM usage.
echo "cleanroom" > "$ROOTFS_DIR/etc/hostname"
cat > "$ROOTFS_DIR/etc/resolv.conf" <<RESOLV
nameserver 1.1.1.1
nameserver 8.8.8.8
RESOLV

host_arch="$(uname -m)"
if [[ "$host_arch" == "$ARCH" ]]; then
  echo "installing base packages in rootfs: git, strace, mise"
  chroot "$ROOTFS_DIR" /bin/sh -lc "apk update && apk add --no-cache git strace mise"
else
  echo "skipping package installation for foreign arch rootfs (host=$host_arch target=$ARCH)"
  echo "- create-rootfs-image still succeeds; install packages later on a matching-arch host"
fi

# Serial console login for local VM debugging.
if [[ -f "$ROOTFS_DIR/etc/inittab" ]] && ! grep -q "ttyS0" "$ROOTFS_DIR/etc/inittab"; then
  cat >> "$ROOTFS_DIR/etc/inittab" <<INITTAB

s0:12345:respawn:/sbin/getty -L ttyS0 115200 vt100
INITTAB
fi

if [[ -f "$OUTPUT" ]]; then
  echo "removing existing output image: $OUTPUT"
  rm -f "$OUTPUT"
fi

echo "creating ext4 image: $OUTPUT (${SIZE_MB} MiB)"
truncate -s "${SIZE_MB}M" "$OUTPUT"
mkfs.ext4 -F "$OUTPUT" >/dev/null

echo "copying rootfs into image"
mount -o loop "$OUTPUT" "$MOUNT_DIR"
cp -a "$ROOTFS_DIR"/. "$MOUNT_DIR"/
sync
umount "$MOUNT_DIR"

echo "rootfs image created successfully"
echo "- image: $OUTPUT"
echo "- tarball: $TARBALL_URL"
echo "- arch: $ARCH"
echo "- size_mib: $SIZE_MB"
if [[ "$TARGET_UID" != "0" ]]; then
  chown "$TARGET_UID:$TARGET_GID" "$OUTPUT"
  echo "- owner: $TARGET_USER"
fi
echo
echo "next step: prepare guest agent"
echo "scripts/prepare-firecracker-image.sh --rootfs-image $OUTPUT"
