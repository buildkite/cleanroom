#!/usr/bin/env bash
set -euo pipefail

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

INSTALL_DIR="${CLEANROOM_GLOBAL_INSTALL_DIR:-/usr/local/bin}"
DIST_DIR="${CLEANROOM_GLOBAL_DIST_DIR:-dist}"
HOST_OS="$(go env GOOS)"
HOST_ARCH="$(go env GOARCH)"
ENTITLEMENTS="cmd/cleanroom-darwin-vz/entitlements.plist"

declare -a SUDO_CMD=()

run_cmd() {
  if [ "${#SUDO_CMD[@]}" -gt 0 ]; then
    "${SUDO_CMD[@]}" "$@"
  else
    "$@"
  fi
}

prepare_install_dir() {
  if [ ! -d "$INSTALL_DIR" ]; then
    if [ "$(id -u)" -eq 0 ]; then
      mkdir -p "$INSTALL_DIR"
    else
      if mkdir -p "$INSTALL_DIR" 2>/dev/null; then
        :
      else
        require_cmd sudo
        SUDO_CMD=(sudo)
        run_cmd mkdir -p "$INSTALL_DIR"
      fi
    fi
  fi

  if [ "$(id -u)" -ne 0 ] && [ ! -w "$INSTALL_DIR" ]; then
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

require_cmd go
prepare_install_dir

CLEANROOM_BIN="${DIST_DIR}/cleanroom"
GUEST_AGENT_LINUX_BIN="${DIST_DIR}/cleanroom-guest-agent-linux-${HOST_ARCH}"
GUEST_AGENT_BIN="${DIST_DIR}/cleanroom-guest-agent"

if [ ! -f "$GUEST_AGENT_BIN" ]; then
  GUEST_AGENT_BIN="$GUEST_AGENT_LINUX_BIN"
fi

install_binary "$CLEANROOM_BIN" "${INSTALL_DIR}/cleanroom"
install_binary "$GUEST_AGENT_LINUX_BIN" "${INSTALL_DIR}/cleanroom-guest-agent-linux-${HOST_ARCH}"
install_binary "$GUEST_AGENT_BIN" "${INSTALL_DIR}/cleanroom-guest-agent"

if [ "$HOST_OS" = "darwin" ]; then
  require_cmd codesign
  HELPER_BIN="${DIST_DIR}/cleanroom-darwin-vz"
  install_binary "$HELPER_BIN" "${INSTALL_DIR}/cleanroom-darwin-vz"
  [ -f "$ENTITLEMENTS" ] || die "missing entitlements file: ${ENTITLEMENTS}"
  run_cmd codesign --force --sign - --entitlements "$ENTITLEMENTS" "${INSTALL_DIR}/cleanroom-darwin-vz"
  log "signed ${INSTALL_DIR}/cleanroom-darwin-vz"
fi
