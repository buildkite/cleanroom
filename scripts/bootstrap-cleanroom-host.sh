#!/usr/bin/env bash
set -euo pipefail

log() {
  printf '[bootstrap-cleanroom-host] %s\n' "$*"
}

die() {
  printf '[bootstrap-cleanroom-host] error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || die "required command not found: ${cmd}"
}

install_sudo_if_missing() {
  if command -v sudo >/dev/null 2>&1; then
    return
  fi

  export DEBIAN_FRONTEND=noninteractive
  apt-get update -y
  apt-get install -y sudo
}

resolve_firecracker_arch() {
  case "$(uname -m)" in
    x86_64|amd64)
      printf 'x86_64'
      ;;
    arm64|aarch64)
      printf 'aarch64'
      ;;
    *)
      die "unsupported architecture for firecracker: $(uname -m)"
      ;;
  esac
}

install_firecracker_binary() {
  local version="$1"
  local arch
  local url
  local tmp_dir
  local binary

  arch="$(resolve_firecracker_arch)"
  url="https://github.com/firecracker-microvm/firecracker/releases/download/v${version}/firecracker-v${version}-${arch}.tgz"

  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' RETURN

  curl -fsSL "$url" -o "$tmp_dir/firecracker.tgz"
  tar -xzf "$tmp_dir/firecracker.tgz" -C "$tmp_dir"

  binary="$(find "$tmp_dir" -type f -name "firecracker-v${version}-${arch}" | head -n 1)"
  [ -n "$binary" ] || die "firecracker binary missing in release archive"

  install -o root -g root -m 0755 "$binary" /usr/local/bin/firecracker
}

if [ "$(id -u)" -ne 0 ]; then
  die "must run as root"
fi

NAME_PREFIX="${CLEANROOM_BOOTSTRAP_NAME_PREFIX:-cleanroom-prod}"
INSTALL_FIRECRACKER="${CLEANROOM_INSTALL_FIRECRACKER:-true}"
FIRECRACKER_VERSION="${CLEANROOM_FIRECRACKER_VERSION:-1.14.2}"
CLEANROOM_BINARY_INSTALL_DIR="${CLEANROOM_BINARY_INSTALL_DIR:-/usr/local/bin}"
CLEANROOM_CONFIG_DIR="${CLEANROOM_CONFIG_DIR:-/root/.config/cleanroom}"
CLEANROOM_FIRECRACKER_VCPUS="${CLEANROOM_FIRECRACKER_VCPUS:-4}"
CLEANROOM_FIRECRACKER_MEMORY_MIB="${CLEANROOM_FIRECRACKER_MEMORY_MIB:-8192}"
CLEANROOM_FIRECRACKER_LAUNCH_SECONDS="${CLEANROOM_FIRECRACKER_LAUNCH_SECONDS:-90}"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"

require_cmd apt-get
require_cmd curl
require_cmd tar
require_cmd systemctl

install_sudo_if_missing

export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y e2fsprogs golang-go iproute2 iptables

if [ "$INSTALL_FIRECRACKER" = "true" ]; then
  log "installing firecracker ${FIRECRACKER_VERSION}"
  install_firecracker_binary "$FIRECRACKER_VERSION"
fi

log "building cleanroom binaries from ${REPO_ROOT}"
export GOTOOLCHAIN=auto
(
  cd "$REPO_ROOT"
  scripts/build-go.sh
)

install -d -o root -g root -m 0755 "$CLEANROOM_BINARY_INSTALL_DIR"
install -o root -g root -m 0755 "$REPO_ROOT/dist/cleanroom" "$CLEANROOM_BINARY_INSTALL_DIR/cleanroom"
install -o root -g root -m 0755 "$REPO_ROOT/dist/cleanroom-guest-agent" "$CLEANROOM_BINARY_INSTALL_DIR/cleanroom-guest-agent"

snapshot_base_dir='/var/lib/cleanroom/snapshots'
CLEANROOM_ZFS_DATASET="${CLEANROOM_ZFS_DATASET:-cleanroom/data}"
if command -v zpool >/dev/null 2>&1 && zpool list cleanroom >/dev/null 2>&1; then
  if command -v zfs >/dev/null 2>&1; then
    dataset_mountpoint="$(zfs get -H -o value mountpoint "$CLEANROOM_ZFS_DATASET" 2>/dev/null || true)"
    case "$dataset_mountpoint" in
      ""|none|legacy|-)
        ;;
      *)
        snapshot_base_dir="$dataset_mountpoint/snapshots"
        ;;
    esac
  fi
fi

install -d -o root -g root -m 0755 "$snapshot_base_dir"
install -d -o root -g root -m 0755 "$CLEANROOM_CONFIG_DIR"

cat > "$CLEANROOM_CONFIG_DIR/config.yaml" <<EOF
default_backend: firecracker
backends:
  firecracker:
    binary_path: /usr/local/bin/firecracker
    vcpus: ${CLEANROOM_FIRECRACKER_VCPUS}
    memory_mib: ${CLEANROOM_FIRECRACKER_MEMORY_MIB}
    launch_seconds: ${CLEANROOM_FIRECRACKER_LAUNCH_SECONDS}
    snapshots:
      enabled: true
      driver: file
      base_dir: ${snapshot_base_dir}
      quiesce_timeout_seconds: 15
EOF

chmod 0644 "$CLEANROOM_CONFIG_DIR/config.yaml"

log "running cleanroom doctor"
"$CLEANROOM_BINARY_INSTALL_DIR/cleanroom" doctor

log "installing cleanroom system daemon"
"$CLEANROOM_BINARY_INSTALL_DIR/cleanroom" daemon install --force --log-level info

systemctl is-active --quiet cleanroom.service || die "cleanroom.service failed to start"

log "cleanroom host ready (${NAME_PREFIX})"
