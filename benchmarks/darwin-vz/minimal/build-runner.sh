#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
OUTPUT_PATH="${1:-${REPO_ROOT}/dist/darwin-vz-minimal}"
ENTITLEMENTS_PATH="${CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS:-${REPO_ROOT}/cmd/cleanroom-darwin-vz/entitlements.plist}"

mkdir -p "$(dirname "${OUTPUT_PATH}")"
xcrun swiftc \
  -O \
  -framework Virtualization \
  "${SCRIPT_DIR}/runner.swift" \
  -o "${OUTPUT_PATH}"

codesign --force --sign "${CLEANROOM_DARWIN_VZ_MINIMAL_SIGN_IDENTITY:--}" \
  --entitlements "${ENTITLEMENTS_PATH}" \
  "${OUTPUT_PATH}"
