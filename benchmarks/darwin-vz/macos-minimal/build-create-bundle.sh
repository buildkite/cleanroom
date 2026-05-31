#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
OUTPUT_PATH="${1:-${REPO_ROOT}/dist/darwin-vz-macos-create-bundle}"
ENTITLEMENTS_PATH="${CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS:-${REPO_ROOT}/cmd/cleanroom-darwin-vz/entitlements.plist}"

mkdir -p "$(dirname "${OUTPUT_PATH}")"
xcrun swiftc \
  -O \
  -framework Virtualization \
  "${SCRIPT_DIR}/create-bundle.swift" \
  -o "${OUTPUT_PATH}"

codesign --force --sign "${CLEANROOM_DARWIN_VZ_MACOS_MINIMAL_SIGN_IDENTITY:--}" \
  --entitlements "${ENTITLEMENTS_PATH}" \
  "${OUTPUT_PATH}"
