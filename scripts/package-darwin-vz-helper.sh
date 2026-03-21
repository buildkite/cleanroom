#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
tmpdir=""

cleanup() {
  if [[ -n "${tmpdir}" ]]; then
    rm -rf "${tmpdir}"
  fi
}

trap cleanup EXIT

SOURCE_PATH="${1:-}"
OUTPUT_PATH="${2:-}"
ENTITLEMENTS_PATH="${CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS:-${REPO_ROOT}/cmd/cleanroom-darwin-vz/entitlements.plist}"
SIGN_IDENTITY="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY:--}"
SIGN_KEYCHAIN="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN:-}"
SIGN_IDENTIFIER="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER:-}"
PROVISION_PROFILE="${CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE:-}"
BUNDLE_MODE="${CLEANROOM_DARWIN_VZ_HELPER_BUNDLE:-}"

[[ -n "${SOURCE_PATH}" ]] || {
  echo "usage: package-darwin-vz-helper.sh <source-path> <output-path>" >&2
  exit 1
}
[[ -n "${OUTPUT_PATH}" ]] || {
  echo "usage: package-darwin-vz-helper.sh <source-path> <output-path>" >&2
  exit 1
}
[[ -e "${SOURCE_PATH}" ]] || {
  echo "missing helper source: ${SOURCE_PATH}" >&2
  exit 1
}
[[ -f "${ENTITLEMENTS_PATH}" ]] || {
  echo "missing entitlements plist: ${ENTITLEMENTS_PATH}" >&2
  exit 1
}
if [[ -n "${PROVISION_PROFILE}" && ! -f "${PROVISION_PROFILE}" ]]; then
  echo "missing provisioning profile: ${PROVISION_PROFILE}" >&2
  exit 1
fi

require_command() {
  local name="$1"
  command -v "$name" >/dev/null 2>&1 || {
    echo "missing required command: $name" >&2
    exit 1
  }
}

copy_file() {
  local src="$1"
  local dst="$2"
  install -m 0755 "${src}" "${dst}"
}

write_info_plist() {
  local path="$1"
  local bundle_identifier="$2"

  cat > "${path}" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleExecutable</key>
  <string>cleanroom-darwin-vz</string>
  <key>CFBundleIdentifier</key>
  <string>${bundle_identifier}</string>
  <key>CFBundleName</key>
  <string>cleanroom-darwin-vz</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>CFBundleShortVersionString</key>
  <string>0.0.0</string>
  <key>CFBundleVersion</key>
  <string>1</string>
</dict>
</plist>
EOF
}

codesign_target() {
  local target="$1"
  local -a args=(
    --force
    --sign "${SIGN_IDENTITY}"
    --entitlements "${ENTITLEMENTS_PATH}"
  )
  if [[ -n "${SIGN_KEYCHAIN}" ]]; then
    args+=(--keychain "${SIGN_KEYCHAIN}")
  fi
  if [[ -n "${SIGN_IDENTIFIER}" ]]; then
    args+=(-i "${SIGN_IDENTIFIER}")
  fi
  args+=("${target}")
  echo "[package-darwin-vz-helper] codesigning ${target}"
  codesign "${args[@]}"
}

require_command codesign
require_command install

source_is_bundle=0
if [[ -d "${SOURCE_PATH}" ]]; then
  source_is_bundle=1
  require_command ditto
  SOURCE_EXECUTABLE_PATH="${SOURCE_PATH}/Contents/MacOS/cleanroom-darwin-vz"
  [[ -f "${SOURCE_EXECUTABLE_PATH}" ]] || {
    echo "missing helper executable in bundle: ${SOURCE_EXECUTABLE_PATH}" >&2
    exit 1
  }
else
  SOURCE_EXECUTABLE_PATH="${SOURCE_PATH}"
  [[ -f "${SOURCE_EXECUTABLE_PATH}" ]] || {
    echo "missing helper binary: ${SOURCE_EXECUTABLE_PATH}" >&2
    exit 1
  }
fi

if [[ -n "${PROVISION_PROFILE}" ]]; then
  BUNDLE_MODE="1"
fi

if [[ -n "${BUNDLE_MODE}" || "${OUTPUT_PATH}" == *.app ]]; then
  if [[ -n "${PROVISION_PROFILE}" && -z "${SIGN_IDENTIFIER}" ]]; then
    echo "CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER is required when embedding a provisioning profile" >&2
    exit 1
  fi

  if [[ "${OUTPUT_PATH}" == *.app ]]; then
    APP_PATH="${OUTPUT_PATH}"
  else
    APP_PATH="${OUTPUT_PATH}.app"
    rm -f "${OUTPUT_PATH}"
  fi
  EXECUTABLE_PATH="${APP_PATH}/Contents/MacOS/cleanroom-darwin-vz"
  INFO_PLIST_PATH="${APP_PATH}/Contents/Info.plist"
  PROFILE_DEST="${APP_PATH}/Contents/embedded.provisionprofile"
  BUNDLE_SOURCE_PATH="${SOURCE_PATH}"

  if [[ "${source_is_bundle}" == "1" && "${SOURCE_PATH}" == "${APP_PATH}" ]]; then
    tmpdir="$(mktemp -d /tmp/cleanroom-darwin-vz-package.XXXXXX)"
    BUNDLE_SOURCE_PATH="${tmpdir}/cleanroom-darwin-vz.app"
    ditto "${SOURCE_PATH}" "${BUNDLE_SOURCE_PATH}"
  fi

  rm -rf "${APP_PATH}"
  mkdir -p "$(dirname "${EXECUTABLE_PATH}")"
  if [[ "${source_is_bundle}" == "1" ]]; then
    ditto "${BUNDLE_SOURCE_PATH}" "${APP_PATH}"
  else
    copy_file "${SOURCE_EXECUTABLE_PATH}" "${EXECUTABLE_PATH}"
  fi
  if [[ "${source_is_bundle}" != "1" || -n "${SIGN_IDENTIFIER}" || ! -f "${INFO_PLIST_PATH}" ]]; then
    BUNDLE_IDENTIFIER="${SIGN_IDENTIFIER:-com.buildkite.cleanroom.darwin-vz}"
    write_info_plist "${INFO_PLIST_PATH}" "${BUNDLE_IDENTIFIER}"
  fi
  if [[ -n "${PROVISION_PROFILE}" ]]; then
    install -m 0644 "${PROVISION_PROFILE}" "${PROFILE_DEST}"
  else
    rm -f "${PROFILE_DEST}"
  fi
  echo "[package-darwin-vz-helper] prepared app bundle at ${APP_PATH}"
  codesign_target "${APP_PATH}"
  exit 0
fi

APP_PATH="${OUTPUT_PATH}.app"
PROFILE_DEST="${OUTPUT_PATH}.provisionprofile"
rm -rf "${APP_PATH}"
mkdir -p "$(dirname "${OUTPUT_PATH}")"
copy_file "${SOURCE_EXECUTABLE_PATH}" "${OUTPUT_PATH}"
echo "[package-darwin-vz-helper] prepared binary at ${OUTPUT_PATH}"
codesign_target "${OUTPUT_PATH}"
if [[ -n "${PROVISION_PROFILE}" ]]; then
  install -m 0644 "${PROVISION_PROFILE}" "${PROFILE_DEST}"
else
  rm -f "${PROFILE_DEST}"
fi
