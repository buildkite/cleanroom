#!/usr/bin/env bash
set -euo pipefail

die() {
  printf '[ci-buildkite-release] error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  local name="$1"
  command -v "$name" >/dev/null 2>&1 || die "missing required command: ${name}"
}

fetch_secret() {
  local key="$1"
  buildkite-agent secret get "$key"
}

normalize_secret_value() {
  printf '%s' "$1" | tr -d '\r'
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

require_command buildkite-agent
require_command git
require_command go
require_command goreleaser

[[ -n "${BUILDKITE_TAG:-}" ]] || die "BUILDKITE_TAG is required for release publishing"

cd "${REPO_ROOT}"
git fetch --tags origin

github_token="$(normalize_secret_value "$(fetch_secret CLEANROOM_GITHUB_RELEASE_TOKEN)")"
[[ -n "${github_token}" ]] || die "CLEANROOM_GITHUB_RELEASE_TOKEN is empty"
export GITHUB_TOKEN="${github_token}"

echo "--- :rocket: Publish GitHub release"
# cleanroom ships as a single CGO-free binary per platform; goreleaser builds
# and uploads the tar.gz archives directly. The runtime (spore) is installed
# separately.
goreleaser release --clean
