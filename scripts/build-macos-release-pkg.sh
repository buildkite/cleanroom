#!/usr/bin/env bash
set -euo pipefail

die() {
  printf '[build-macos-release-pkg] error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || die "required command not found: ${cmd}"
}

usage() {
  cat <<'USAGE'
Build a macOS installer package for Cleanroom.

Usage:
  build-macos-release-pkg.sh <output.pkg>

Required environment:
  CLEANROOM_MACOS_RELEASE_VERSION                  Package version (for example 0.1.0)
  CLEANROOM_MACOS_RELEASE_CLEANROOM_BINARY        Path to the signed or unsigned cleanroom macOS binary
  CLEANROOM_MACOS_RELEASE_GUEST_AGENT_BINARY      Path to the Linux cleanroom-guest-agent binary
  CLEANROOM_MACOS_RELEASE_HELPER_APP              Path to the cleanroom-darwin-vz.app bundle

Optional environment:
  CLEANROOM_MACOS_RELEASE_INSTALL_PREFIX          Install prefix inside the package (default: /usr/local/bin)
  CLEANROOM_MACOS_RELEASE_APPLICATION_SIGN_IDENTITY
                                                  Developer ID Application identity for cleanroom (default: ad-hoc "-")
  CLEANROOM_MACOS_RELEASE_INSTALLER_SIGN_IDENTITY Developer ID Installer identity for the package
USAGE
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

[[ $# -eq 1 ]] || {
  usage >&2
  exit 1
}

OUTPUT_PATH="$1"
VERSION="${CLEANROOM_MACOS_RELEASE_VERSION:-}"
CLEANROOM_BINARY="${CLEANROOM_MACOS_RELEASE_CLEANROOM_BINARY:-}"
GUEST_AGENT_BINARY="${CLEANROOM_MACOS_RELEASE_GUEST_AGENT_BINARY:-}"
HELPER_APP="${CLEANROOM_MACOS_RELEASE_HELPER_APP:-}"
INSTALL_PREFIX="${CLEANROOM_MACOS_RELEASE_INSTALL_PREFIX:-/usr/local/bin}"
APPLICATION_SIGN_IDENTITY="${CLEANROOM_MACOS_RELEASE_APPLICATION_SIGN_IDENTITY:-}"
INSTALLER_SIGN_IDENTITY="${CLEANROOM_MACOS_RELEASE_INSTALLER_SIGN_IDENTITY:-}"
if [[ -z "${APPLICATION_SIGN_IDENTITY}" ]]; then
  APPLICATION_SIGN_IDENTITY="-"
fi

[[ -n "${VERSION}" ]] || die "CLEANROOM_MACOS_RELEASE_VERSION is required"
[[ -f "${CLEANROOM_BINARY}" ]] || die "missing cleanroom binary: ${CLEANROOM_BINARY}"
[[ -f "${GUEST_AGENT_BINARY}" ]] || die "missing cleanroom-guest-agent binary: ${GUEST_AGENT_BINARY}"
[[ -d "${HELPER_APP}" ]] || die "missing helper app bundle: ${HELPER_APP}"
[[ "${INSTALL_PREFIX}" = /* ]] || die "install prefix must be absolute: ${INSTALL_PREFIX}"

require_cmd codesign
require_cmd ditto
require_cmd install
require_cmd pkgutil
require_cmd pkgbuild

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

PAYLOAD_ROOT="${WORK_DIR}/payload"
PAYLOAD_BIN_DIR="${PAYLOAD_ROOT}${INSTALL_PREFIX}"
PAYLOAD_HELPER_DIR="${PAYLOAD_BIN_DIR}/cleanroom-darwin-vz.app"
PAYLOAD_CLEANROOM_PATH="${PAYLOAD_BIN_DIR}/cleanroom"
PAYLOAD_GUEST_AGENT_PATH="${PAYLOAD_BIN_DIR}/cleanroom-guest-agent"

mkdir -p "${PAYLOAD_BIN_DIR}" "$(dirname "${OUTPUT_PATH}")"
install -m 0755 "${CLEANROOM_BINARY}" "${PAYLOAD_CLEANROOM_PATH}"
install -m 0755 "${GUEST_AGENT_BINARY}" "${PAYLOAD_GUEST_AGENT_PATH}"
ditto "${HELPER_APP}" "${PAYLOAD_HELPER_DIR}"

codesign_args=(
  --force
  --sign "${APPLICATION_SIGN_IDENTITY}"
)
if [[ "${APPLICATION_SIGN_IDENTITY}" != "-" ]]; then
  codesign_args+=(--options runtime --timestamp)
fi
codesign_args+=("${PAYLOAD_CLEANROOM_PATH}")
codesign "${codesign_args[@]}"

codesign --verify --strict --verbose=2 "${PAYLOAD_CLEANROOM_PATH}"
codesign --verify --deep --strict --verbose=2 "${PAYLOAD_HELPER_DIR}"

pkgbuild_args=(
  --root "${PAYLOAD_ROOT}"
  --identifier "com.buildkite.cleanroom"
  --version "${VERSION}"
  --install-location /
)
if [[ -n "${INSTALLER_SIGN_IDENTITY}" ]]; then
  pkgbuild_args+=(--sign "${INSTALLER_SIGN_IDENTITY}")
fi
pkgbuild_args+=("${OUTPUT_PATH}")
pkgbuild "${pkgbuild_args[@]}"
if [[ -n "${INSTALLER_SIGN_IDENTITY}" ]]; then
  pkgutil --check-signature "${OUTPUT_PATH}"
fi
