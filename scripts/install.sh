#!/usr/bin/env bash
set -euo pipefail

# Install the cleanroom CLI from GitHub releases. cleanroom is a single
# CGO-free binary; the VM runtime (spore) is installed separately.

log() { printf '[cleanroom-install] %s\n' "$*"; }
die() {
  printf '[cleanroom-install] error: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'USAGE'
Install the cleanroom CLI from GitHub releases.

Usage:
  install.sh [--version <version>] [--install-dir <dir>] [--repo <owner/repo>]

  curl -fsSL https://raw.githubusercontent.com/buildkite/cleanroom/main/scripts/install.sh | bash
  curl -fsSL https://raw.githubusercontent.com/buildkite/cleanroom/main/scripts/install.sh | \
    bash -s -- --version vX.Y.Z

Environment variables:
  CLEANROOM_VERSION      Release version to install (default: latest)
  CLEANROOM_INSTALL_DIR  Install destination (default: /usr/local/bin)
  CLEANROOM_REPO         GitHub repo in owner/repo format (default: buildkite/cleanroom)
USAGE
}

VERSION="${CLEANROOM_VERSION:-}"
INSTALL_DIR="${CLEANROOM_INSTALL_DIR:-/usr/local/bin}"
REPO="${CLEANROOM_REPO:-buildkite/cleanroom}"

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --install-dir) INSTALL_DIR="$2"; shift 2 ;;
    --repo) REPO="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage; die "unknown argument: $1" ;;
  esac
done

require_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }
require_cmd curl
require_cmd tar
require_cmd uname

detect_os() {
  case "$(uname -s)" in
    Darwin) echo "Darwin" ;;
    Linux) echo "Linux" ;;
    *) die "unsupported OS: $(uname -s)" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "x86_64" ;;
    arm64|aarch64) echo "arm64" ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

OS="$(detect_os)"
ARCH="$(detect_arch)"
API_BASE="https://api.github.com/repos/${REPO}"

if [ -z "$VERSION" ]; then
  log "Resolving latest release"
  VERSION="$(curl -fsSL "${API_BASE}/releases/latest" | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
  [ -n "$VERSION" ] || die "could not resolve latest release version"
fi
log "Installing cleanroom ${VERSION} (${OS}/${ARCH}) to ${INSTALL_DIR}"

asset="cleanroom_${OS}_${ARCH}.tar.gz"
base_url="https://github.com/${REPO}/releases/download/${VERSION}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL -o "${tmp}/${asset}" "${base_url}/${asset}" || die "failed to download ${asset}"
curl -fsSL -o "${tmp}/checksums.txt" "${base_url}/checksums.txt" || die "failed to download checksums.txt; refusing to install unverified binaries"
expected="$(awk -v name="$asset" '$2 == name {print $1}' "${tmp}/checksums.txt")"
[ -n "$expected" ] || die "checksum for ${asset} not found"
actual="$(sha256_file "${tmp}/${asset}")"
[ "$expected" = "$actual" ] || die "checksum mismatch for ${asset}"
log "Verified checksum"

tar -xzf "${tmp}/${asset}" -C "$tmp"
[ -f "${tmp}/cleanroom" ] || die "archive did not contain the cleanroom binary"
chmod +x "${tmp}/cleanroom"

install_cmd() {
  if [ -w "$INSTALL_DIR" ] || mkdir -p "$INSTALL_DIR" 2>/dev/null && [ -w "$INSTALL_DIR" ]; then
    "$@"
  elif command -v sudo >/dev/null 2>&1; then
    sudo "$@"
  else
    die "cannot write to ${INSTALL_DIR} and sudo is unavailable"
  fi
}

install_cmd install -d "$INSTALL_DIR"
install_cmd install -m 0755 "${tmp}/cleanroom" "${INSTALL_DIR}/cleanroom"

log "Installed cleanroom to ${INSTALL_DIR}/cleanroom"
log "The VM runtime is provided by spore; install it separately (see the cleanroom README)."
