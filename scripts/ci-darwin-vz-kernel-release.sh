#!/usr/bin/env bash
set -euo pipefail

die() {
  printf '[ci-darwin-vz-kernel-release] error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  local name="$1"
  command -v "$name" >/dev/null 2>&1 || die "missing required command: ${name}"
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
KERNEL_RELEASE_DIR="${REPO_ROOT}/release-extra/kernels"

require_command buildkite-agent
require_command docker
require_command git
require_command python3
require_command tar

cd "${REPO_ROOT}"
rm -rf "${KERNEL_RELEASE_DIR}"

echo "--- :penguin: Build darwin-vz minimal kernel release assets"
"${SCRIPT_DIR}/build-darwin-vz-minimal-kernel-release.sh" "${KERNEL_RELEASE_DIR}"

echo "--- :package: Upload darwin-vz kernel release artifacts"
tar -C "${REPO_ROOT}/release-extra" -czf "${REPO_ROOT}/release-extra/kernels.tar.gz" kernels
buildkite-agent artifact upload "release-extra/kernels.tar.gz"
buildkite-agent artifact upload "release-extra/kernels/*"
