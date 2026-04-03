#!/usr/bin/env bash
set -euo pipefail

die() {
  printf '[notarize-macos-package] error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || die "required command not found: ${cmd}"
}

usage() {
  cat <<'USAGE'
Submit a signed macOS package for notarization and staple the result.

Usage:
  notarize-macos-package.sh <package.pkg>

Required environment:
  CLEANROOM_MACOS_NOTARY_KEYCHAIN_PROFILE
                                    Keychain profile created with `notarytool store-credentials`
  or:
  CLEANROOM_MACOS_NOTARY_KEY_PATH   Path to an App Store Connect API key (.p8)
  CLEANROOM_MACOS_NOTARY_KEY_ID     App Store Connect API key ID
  CLEANROOM_MACOS_NOTARY_ISSUER_ID  App Store Connect issuer ID

Optional environment:
  CLEANROOM_MACOS_NOTARY_KEYCHAIN_PATH
                                    Keychain path when using `CLEANROOM_MACOS_NOTARY_KEYCHAIN_PROFILE`
  CLEANROOM_MACOS_NOTARY_TIMEOUT    notarytool timeout (default: 15m)
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

PACKAGE_PATH="$1"
KEYCHAIN_PROFILE="${CLEANROOM_MACOS_NOTARY_KEYCHAIN_PROFILE:-}"
KEYCHAIN_PATH="${CLEANROOM_MACOS_NOTARY_KEYCHAIN_PATH:-}"
KEY_PATH="${CLEANROOM_MACOS_NOTARY_KEY_PATH:-}"
KEY_ID="${CLEANROOM_MACOS_NOTARY_KEY_ID:-}"
ISSUER_ID="${CLEANROOM_MACOS_NOTARY_ISSUER_ID:-}"
TIMEOUT="${CLEANROOM_MACOS_NOTARY_TIMEOUT:-15m}"

[[ -f "${PACKAGE_PATH}" ]] || die "missing package: ${PACKAGE_PATH}"

require_cmd xcrun

notarytool_args=(
  submit
  "${PACKAGE_PATH}"
  --wait
  --timeout "${TIMEOUT}"
)
if [[ -n "${KEYCHAIN_PROFILE}" ]]; then
  notarytool_args+=(--keychain-profile "${KEYCHAIN_PROFILE}")
  if [[ -n "${KEYCHAIN_PATH}" ]]; then
    notarytool_args+=(--keychain "${KEYCHAIN_PATH}")
  fi
else
  [[ -f "${KEY_PATH}" ]] || die "missing App Store Connect API key: ${KEY_PATH}"
  [[ -n "${KEY_ID}" ]] || die "CLEANROOM_MACOS_NOTARY_KEY_ID is required"
  [[ -n "${ISSUER_ID}" ]] || die "CLEANROOM_MACOS_NOTARY_ISSUER_ID is required"
  notarytool_args+=(
    --key "${KEY_PATH}"
    --key-id "${KEY_ID}"
    --issuer "${ISSUER_ID}"
  )
fi

xcrun notarytool "${notarytool_args[@]}"

xcrun stapler staple -v "${PACKAGE_PATH}"
xcrun stapler validate -v "${PACKAGE_PATH}"
