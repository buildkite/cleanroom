#!/usr/bin/env bash
set -euo pipefail

log() {
  printf '[cleanroom-install] %s\n' "$*"
}

warn() {
  printf '[cleanroom-install] warning: %s\n' "$*" >&2
}

die() {
  printf '[cleanroom-install] error: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'USAGE'
Install cleanroom from GitHub releases.

Usage:
  install.sh [--version <version>] [--install-dir <dir>] [--repo <owner/repo>] [--no-darwin-helper]

Examples:
  curl -fsSL https://raw.githubusercontent.com/buildkite/cleanroom/main/scripts/install.sh | bash
  curl -fsSL https://raw.githubusercontent.com/buildkite/cleanroom/main/scripts/install.sh | \
    bash -s -- --version vX.Y.Z

Environment variables:
  CLEANROOM_VERSION               Optional release version (example: vX.Y.Z)
  CLEANROOM_INSTALL_DIR           Install destination (default: /usr/local/bin)
  CLEANROOM_REPO                  GitHub repo in owner/repo format (default: buildkite/cleanroom)
  CLEANROOM_INSTALL_DARWIN_HELPER Set to 0 to skip cleanroom-darwin-vz install on macOS
USAGE
}

require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || die "required command not found: ${cmd}"
}

sha256_file() {
  local file="$1"

  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
    return
  fi

  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print $1}'
    return
  fi

  die "sha256 tool not found (need sha256sum or shasum)"
}

download() {
  local url="$1"
  local dest="$2"
  if ! curl -fsSL --retry 3 --connect-timeout 10 "$url" -o "$dest"; then
    die "failed to download ${url}"
  fi
}

normalize_version() {
  local raw="$1"
  if [ -z "$raw" ] || [ "$raw" = "latest" ]; then
    printf 'latest'
    return
  fi

  case "$raw" in
    v*) printf '%s' "$raw" ;;
    *) printf 'v%s' "$raw" ;;
  esac
}

lookup_checksum() {
  local asset="$1"
  local checksums_file="$2"
  local checksum

  checksum=$(awk -v name="$asset" '$2 == name { print $1 }' "$checksums_file")
  if [ -z "$checksum" ]; then
    die "checksum for ${asset} not found in ${checksums_file}"
  fi

  printf '%s' "$checksum"
}

verify_asset_against_checksums() {
  local asset="$1"
  local asset_path="$2"
  local checksums_file="$3"
  local expected actual

  expected=$(lookup_checksum "$asset" "$checksums_file")
  actual=$(sha256_file "$asset_path")

  if [ "$expected" != "$actual" ]; then
    die "checksum mismatch for ${asset}"
  fi
}

extract_binary() {
  local archive="$1"
  local output_dir="$2"
  mkdir -p "$output_dir"
  tar -xzf "$archive" -C "$output_dir"
}

declare -a SUDO_CMD=()

prepare_install_dir() {
  if [ ! -d "$INSTALL_DIR" ]; then
    if [ "$(id -u)" -eq 0 ]; then
      mkdir -p "$INSTALL_DIR"
    else
      if mkdir -p "$INSTALL_DIR" 2>/dev/null; then
        :
      else
        command -v sudo >/dev/null 2>&1 || die "${INSTALL_DIR} does not exist and sudo is unavailable"
        SUDO_CMD=(sudo)
        "${SUDO_CMD[@]}" mkdir -p "$INSTALL_DIR"
      fi
    fi
  fi

  if [ "$(id -u)" -ne 0 ] && [ ! -w "$INSTALL_DIR" ]; then
    command -v sudo >/dev/null 2>&1 || die "${INSTALL_DIR} is not writable and sudo is unavailable"
    SUDO_CMD=(sudo)
  fi
}

install_binary() {
  local src="$1"
  local dst="$2"
  "${SUDO_CMD[@]}" install -m 0755 "$src" "$dst"
}

HOST_OS_RAW="$(uname -s)"
HOST_ARCH_RAW="$(uname -m)"

case "$HOST_OS_RAW" in
  Linux) HOST_OS="Linux" ;;
  Darwin) HOST_OS="Darwin" ;;
  *) die "unsupported operating system: ${HOST_OS_RAW}" ;;
esac

case "$HOST_ARCH_RAW" in
  x86_64|amd64) HOST_ARCH="x86_64" ;;
  arm64|aarch64) HOST_ARCH="arm64" ;;
  *) die "unsupported architecture: ${HOST_ARCH_RAW}" ;;
esac

VERSION="${CLEANROOM_VERSION:-}"
INSTALL_DIR="${CLEANROOM_INSTALL_DIR:-/usr/local/bin}"
REPO="${CLEANROOM_REPO:-buildkite/cleanroom}"
INSTALL_DARWIN_HELPER="${CLEANROOM_INSTALL_DARWIN_HELPER:-1}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      [ "$#" -ge 2 ] || die "--version requires a value"
      VERSION="$2"
      shift 2
      ;;
    --install-dir)
      [ "$#" -ge 2 ] || die "--install-dir requires a value"
      INSTALL_DIR="$2"
      shift 2
      ;;
    --repo)
      [ "$#" -ge 2 ] || die "--repo requires a value"
      REPO="$2"
      shift 2
      ;;
    --no-darwin-helper)
      INSTALL_DARWIN_HELPER=0
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1"
      ;;
  esac
done

require_cmd curl
require_cmd tar
require_cmd awk

VERSION="$(normalize_version "$VERSION")"
if [ "$VERSION" = "latest" ]; then
  RELEASE_BASE="https://github.com/${REPO}/releases/latest/download"
  RELEASE_LABEL="latest"
else
  RELEASE_BASE="https://github.com/${REPO}/releases/download/${VERSION}"
  RELEASE_LABEL="$VERSION"
fi

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT
DARWIN_HELPER_INSTALLED=0

log "Installing cleanroom from ${REPO} (${RELEASE_LABEL}) for ${HOST_OS}/${HOST_ARCH}"

CHECKSUMS_PATH="${WORK_DIR}/checksums.txt"
download "${RELEASE_BASE}/checksums.txt" "$CHECKSUMS_PATH"

CLEANROOM_ASSET="cleanroom_${HOST_OS}_${HOST_ARCH}.tar.gz"
CLEANROOM_ARCHIVE_PATH="${WORK_DIR}/${CLEANROOM_ASSET}"

download "${RELEASE_BASE}/${CLEANROOM_ASSET}" "$CLEANROOM_ARCHIVE_PATH"
verify_asset_against_checksums "$CLEANROOM_ASSET" "$CLEANROOM_ARCHIVE_PATH" "$CHECKSUMS_PATH"

CLEANROOM_EXTRACT_DIR="${WORK_DIR}/cleanroom"
extract_binary "$CLEANROOM_ARCHIVE_PATH" "$CLEANROOM_EXTRACT_DIR"
[ -f "${CLEANROOM_EXTRACT_DIR}/cleanroom" ] || die "cleanroom binary missing in ${CLEANROOM_ASSET}"
[ -f "${CLEANROOM_EXTRACT_DIR}/cleanroom-guest-agent" ] || die "cleanroom-guest-agent missing in ${CLEANROOM_ASSET}"

prepare_install_dir
install_binary "${CLEANROOM_EXTRACT_DIR}/cleanroom" "${INSTALL_DIR}/cleanroom"
install_binary "${CLEANROOM_EXTRACT_DIR}/cleanroom-guest-agent" "${INSTALL_DIR}/cleanroom-guest-agent"

if [ "$HOST_OS" = "Darwin" ] && [ "$INSTALL_DARWIN_HELPER" != "0" ]; then
  require_cmd codesign
  [ -f "${CLEANROOM_EXTRACT_DIR}/cleanroom-darwin-vz" ] || die "cleanroom-darwin-vz missing in ${CLEANROOM_ASSET}"
  [ -f "${CLEANROOM_EXTRACT_DIR}/entitlements.plist" ] || die "entitlements.plist missing in ${CLEANROOM_ASSET}"

  install_binary "${CLEANROOM_EXTRACT_DIR}/cleanroom-darwin-vz" "${INSTALL_DIR}/cleanroom-darwin-vz"
  "${SUDO_CMD[@]}" codesign --force --sign - \
    --entitlements "${CLEANROOM_EXTRACT_DIR}/entitlements.plist" \
    "${INSTALL_DIR}/cleanroom-darwin-vz"
  DARWIN_HELPER_INSTALLED=1
fi

log "Installed cleanroom to ${INSTALL_DIR}/cleanroom"
log "Installed cleanroom-guest-agent to ${INSTALL_DIR}/cleanroom-guest-agent"
if [ "$DARWIN_HELPER_INSTALLED" = "1" ]; then
  log "Installed cleanroom-darwin-vz to ${INSTALL_DIR}/cleanroom-darwin-vz"
fi

case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *) warn "${INSTALL_DIR} is not in PATH" ;;
esac
