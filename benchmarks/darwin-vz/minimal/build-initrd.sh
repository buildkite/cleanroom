#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
OUTPUT_PATH="${1:-${REPO_ROOT}/dist/darwin-vz-minimal-initrd.cpio.gz}"
ROOT_DIR="${REPO_ROOT}/dist/darwin-vz-minimal-initrd-root"

host_arch="$(go env GOARCH)"

rm -rf "${ROOT_DIR}"
mkdir -p \
  "${ROOT_DIR}/bin" \
  "${ROOT_DIR}/dev" \
  "${ROOT_DIR}/proc" \
  "${ROOT_DIR}/root" \
  "${ROOT_DIR}/run" \
  "${ROOT_DIR}/sys" \
  "${ROOT_DIR}/tmp"

GOOS=linux GOARCH="${host_arch}" CGO_ENABLED=0 go build \
  -trimpath \
  -ldflags "-s -w" \
  -o "${ROOT_DIR}/init" \
  "${SCRIPT_DIR}/initrd-agent"

GOOS=linux GOARCH="${host_arch}" CGO_ENABLED=0 go build \
  -trimpath \
  -ldflags "-s -w" \
  -o "${ROOT_DIR}/bin/true" \
  "${SCRIPT_DIR}/initrd-true"

chmod 0755 "${ROOT_DIR}/init" "${ROOT_DIR}/bin/true"
chmod 1777 "${ROOT_DIR}/tmp"

mkdir -p "$(dirname "${OUTPUT_PATH}")"
(
  cd "${ROOT_DIR}"
  find . -print | LC_ALL=C sort | cpio -o -H newc 2>/dev/null | gzip -n >"${OUTPUT_PATH}"
)

printf '%s\n' "${OUTPUT_PATH}"
