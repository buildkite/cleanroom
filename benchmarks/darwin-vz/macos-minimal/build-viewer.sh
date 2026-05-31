#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
OUTPUT_PATH="${1:-${REPO_ROOT}/dist/darwin-vz-macos-viewer}"
ENTITLEMENTS_PATH="${CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS:-${REPO_ROOT}/cmd/cleanroom-darwin-vz/entitlements.plist}"

mkdir -p "$(dirname "${OUTPUT_PATH}")"

swiftc \
  -O \
  -framework AppKit \
  -framework Virtualization \
  "${REPO_ROOT}/benchmarks/darwin-vz/macos-minimal/viewer.swift" \
  -o "${OUTPUT_PATH}"

codesign --force --sign - --entitlements "${ENTITLEMENTS_PATH}" "${OUTPUT_PATH}" >/dev/null

echo "${OUTPUT_PATH}"
