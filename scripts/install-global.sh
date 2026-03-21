#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/dist-layout.sh
source "${SCRIPT_DIR}/dist-layout.sh"

log() {
  printf '[install:global] %s\n' "$*"
}

die() {
  printf '[install:global] error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || die "required command not found: ${cmd}"
}

default_prefix() {
  if [[ "$(id -u)" -eq 0 ]]; then
    printf '/usr/local\n'
    return
  fi
  printf '%s/.local\n' "$HOME"
}

PREFIX="${CLEANROOM_GLOBAL_PREFIX:-}"
if [[ -z "$PREFIX" && -n "${CLEANROOM_GLOBAL_INSTALL_DIR:-}" ]]; then
  case "${CLEANROOM_GLOBAL_INSTALL_DIR}" in
    */bin) PREFIX="${CLEANROOM_GLOBAL_INSTALL_DIR%/bin}" ;;
    *) die "CLEANROOM_GLOBAL_INSTALL_DIR must end with /bin; use CLEANROOM_GLOBAL_PREFIX instead" ;;
  esac
fi
PREFIX="${PREFIX:-$(default_prefix)}"
DIST_DIR="${CLEANROOM_GLOBAL_DIST_DIR:-}"
if [[ -z "$DIST_DIR" ]]; then
  DIST_DIR="$(cleanroom_stage_dir)"
fi
BIN_DIR="$(cleanroom_prefix_bin_dir "$PREFIX")"
LIBEXEC_DIR="$(cleanroom_prefix_libexec_dir "$PREFIX")"

HOST_OS="$(go env GOOS)"
HOST_ARCH="$(go env GOARCH)"

declare -a SUDO_CMD=()

run_cmd() {
  if [ "${#SUDO_CMD[@]}" -gt 0 ]; then
    "${SUDO_CMD[@]}" "$@"
  else
    "$@"
  fi
}

prepare_prefix_dirs() {
  for dir in "$BIN_DIR" "$LIBEXEC_DIR"; do
    if [ ! -d "$dir" ]; then
      if [ "$(id -u)" -eq 0 ]; then
        mkdir -p "$dir"
      else
        if mkdir -p "$dir" 2>/dev/null; then
          :
        else
          require_cmd sudo
          SUDO_CMD=(sudo)
          run_cmd mkdir -p "$dir"
        fi
      fi
    fi
  done

  if [ "$(id -u)" -ne 0 ] && { [ ! -w "$BIN_DIR" ] || [ ! -w "$LIBEXEC_DIR" ]; }; then
    require_cmd sudo
    SUDO_CMD=(sudo)
  fi
}

install_binary() {
  local src="$1"
  local dst="$2"

  [ -f "$src" ] || die "missing build artifact: ${src}"
  run_cmd install -m 0755 "$src" "$dst"
  log "installed $(basename "$dst") to $dst"
}

install_optional_binary() {
  local src="$1"
  local dst="$2"

  [ -f "$src" ] || return 0
  install_binary "$src" "$dst"
}

install_file() {
  local src="$1"
  local dst="$2"

  [ -f "$src" ] || die "missing file artifact: ${src}"
  run_cmd install -m 0644 "$src" "$dst"
  log "installed $(basename "$dst") to $dst"
}

install_optional_file() {
  local src="$1"
  local dst="$2"

  [ -f "$src" ] || return 0
  install_file "$src" "$dst"
}

install_app_bundle() {
  local src="$1"
  local dst="$2"

  [ -d "$src" ] || die "missing app bundle: ${src}"
  run_cmd rm -rf "$dst"
  if [ "${#SUDO_CMD[@]}" -gt 0 ]; then
    run_cmd ditto "$src" "$dst"
  else
    ditto "$src" "$dst"
  fi
  log "installed $(basename "$dst") to $dst"
}

require_cmd go
prepare_prefix_dirs

CLEANROOM_BIN="${DIST_DIR}/bin/cleanroom"
DOWNLOAD_HELPER_BIN="${DIST_DIR}/bin/download-sandbox-file"
GUEST_AGENT_LINUX_BIN="${DIST_DIR}/libexec/cleanroom/cleanroom-guest-agent-linux-${HOST_ARCH}"

install_binary "$CLEANROOM_BIN" "${BIN_DIR}/cleanroom"
install_optional_binary "$DOWNLOAD_HELPER_BIN" "${BIN_DIR}/download-sandbox-file"
install_binary "$GUEST_AGENT_LINUX_BIN" "${LIBEXEC_DIR}/cleanroom-guest-agent-linux-${HOST_ARCH}"

if [ "$HOST_OS" = "darwin" ]; then
  require_cmd ditto
  HELPER_APP="${DIST_DIR}/libexec/cleanroom/cleanroom-darwin-vz.app"
  if [ -d "$HELPER_APP" ]; then
    install_app_bundle "$HELPER_APP" "${LIBEXEC_DIR}/cleanroom-darwin-vz.app"
  fi
  install_optional_file "${DIST_DIR}/libexec/cleanroom/entitlements.plist" "${LIBEXEC_DIR}/entitlements.plist"
  install_optional_file "${DIST_DIR}/libexec/cleanroom/entitlements-vmnet.plist" "${LIBEXEC_DIR}/entitlements-vmnet.plist"
fi
