#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
OUT="${1:-"${ROOT_DIR}/dist/cleanroom-macos-guest-agent"}"

mkdir -p "$(dirname "${OUT}")"

(
  cd "${ROOT_DIR}"
  GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "${OUT}" ./cmd/cleanroom-macos-guest-agent
)

if command -v codesign >/dev/null 2>&1; then
  codesign --force --sign - "${OUT}" >/dev/null
fi

echo "${OUT}"
