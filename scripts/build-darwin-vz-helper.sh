#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

OUTPUT_PATH="${1:-${REPO_ROOT}/dist/cleanroom-darwin-vz}"
SWIFT_TARGET="${CLEANROOM_DARWIN_VZ_HELPER_SWIFT_TARGET:-}"
ENTITLEMENTS_PATH="${CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS:-${REPO_ROOT}/cmd/cleanroom-darwin-vz/entitlements.plist}"
SIGN_IDENTITY="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY:--}"
SIGN_IDENTIFIER="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER:-}"
PROVISION_PROFILE="${CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE:-}"
BUNDLE_MODE="${CLEANROOM_DARWIN_VZ_HELPER_BUNDLE:-}"
SIGN_RUNTIME="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_RUNTIME:-}"

[[ -f "${REPO_ROOT}/cmd/cleanroom-darwin-vz/main.swift" ]] || {
  echo "missing helper source: ${REPO_ROOT}/cmd/cleanroom-darwin-vz/main.swift" >&2
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

mkdir -p "$(dirname "${OUTPUT_PATH}")"

swiftc_args=(
  -O
  -framework Virtualization
  -framework vmnet
)
if [[ -n "${SWIFT_TARGET}" ]]; then
  swiftc_args+=(-target "${SWIFT_TARGET}")
fi
if [[ -n "${PROVISION_PROFILE}" ]]; then
  BUNDLE_MODE="1"
fi

if [[ -n "${BUNDLE_MODE}" ]]; then
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
  BUNDLE_IDENTIFIER="${SIGN_IDENTIFIER:-com.buildkite.cleanroom.darwin-vz}"

  mkdir -p "$(dirname "${EXECUTABLE_PATH}")"
  cat > "${INFO_PLIST_PATH}" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleExecutable</key>
  <string>cleanroom-darwin-vz</string>
  <key>CFBundleIdentifier</key>
  <string>${BUNDLE_IDENTIFIER}</string>
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
  if [[ -n "${PROVISION_PROFILE}" ]]; then
    install -m 0644 "${PROVISION_PROFILE}" "${PROFILE_DEST}"
  else
    rm -f "${PROFILE_DEST}"
  fi
  swiftc_args+=(
    "${REPO_ROOT}/cmd/cleanroom-darwin-vz/main.swift"
    -o "${EXECUTABLE_PATH}"
  )
  xcrun swiftc "${swiftc_args[@]}"

  codesign_args=(
    --force
    --sign "${SIGN_IDENTITY}"
    --entitlements "${ENTITLEMENTS_PATH}"
  )
  if [[ -n "${SIGN_RUNTIME}" && "${SIGN_IDENTITY}" != "-" ]]; then
    codesign_args+=(--options runtime --timestamp)
  fi
  if [[ -n "${SIGN_IDENTIFIER}" ]]; then
    codesign_args+=(-i "${SIGN_IDENTIFIER}")
  fi
  codesign_args+=("${APP_PATH}")
  codesign "${codesign_args[@]}"
else
  APP_PATH="${OUTPUT_PATH}.app"
  EXECUTABLE_PATH="${OUTPUT_PATH}"
  PROFILE_DEST="${OUTPUT_PATH}.provisionprofile"
  rm -rf "${APP_PATH}"
  swiftc_args+=(
    "${REPO_ROOT}/cmd/cleanroom-darwin-vz/main.swift"
    -o "${EXECUTABLE_PATH}"
  )
  xcrun swiftc "${swiftc_args[@]}"

  codesign_args=(
    --force
    --sign "${SIGN_IDENTITY}"
    --entitlements "${ENTITLEMENTS_PATH}"
  )
  if [[ -n "${SIGN_RUNTIME}" && "${SIGN_IDENTITY}" != "-" ]]; then
    codesign_args+=(--options runtime --timestamp)
  fi
  if [[ -n "${SIGN_IDENTIFIER}" ]]; then
    codesign_args+=(-i "${SIGN_IDENTIFIER}")
  fi
  codesign_args+=("${EXECUTABLE_PATH}")
  codesign "${codesign_args[@]}"

  if [[ -n "${PROVISION_PROFILE}" ]]; then
    install -m 0644 "${PROVISION_PROFILE}" "${PROFILE_DEST}"
  else
    rm -f "${PROFILE_DEST}"
  fi
fi
