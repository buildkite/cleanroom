#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
OUTPUT_PATH="${1:-${REPO_ROOT}/dist/darwin-vz-macos-helper-runner}"

mkdir -p "$(dirname "${OUTPUT_PATH}")"
go build -o "${OUTPUT_PATH}" "${REPO_ROOT}/benchmarks/darwin-vz/macos-minimal"
