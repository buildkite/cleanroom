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
RELEASE_EXTRA_DIR="${REPO_ROOT}/release-extra"
GITHUB_REPOSITORY_NAME="${CLEANROOM_GITHUB_REPOSITORY:-buildkite/cleanroom}"
GITHUB_API_BASE="https://api.github.com/repos/${GITHUB_REPOSITORY_NAME}"

download_darwin_release_artifacts() {
  local archive_name archive_path

  mkdir -p "${RELEASE_EXTRA_DIR}"
  rm -rf "${RELEASE_EXTRA_DIR}/darwin_arm64" "${RELEASE_EXTRA_DIR}/darwin_amd64"
  rm -f "${RELEASE_EXTRA_DIR}/darwin_arm64.tar.gz" "${RELEASE_EXTRA_DIR}/darwin_amd64.tar.gz"

  buildkite-agent artifact download "release-extra/darwin_*.tar.gz" "${REPO_ROOT}"

  for archive_name in darwin_arm64 darwin_amd64; do
    archive_path="${RELEASE_EXTRA_DIR}/${archive_name}.tar.gz"
    [[ -f "${archive_path}" ]] || die "missing downloaded Darwin release archive: ${archive_path}"
    tar -xzf "${archive_path}" -C "${RELEASE_EXTRA_DIR}"
  done
}

build_linux_release_extras() {
  mkdir -p "${RELEASE_EXTRA_DIR}/linux_amd64" "${RELEASE_EXTRA_DIR}/linux_arm64"

  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
    -o "${RELEASE_EXTRA_DIR}/linux_amd64/cleanroom-guest-agent" ./cmd/cleanroom-guest-agent
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
    -o "${RELEASE_EXTRA_DIR}/linux_arm64/cleanroom-guest-agent" ./cmd/cleanroom-guest-agent
}

publish_github_release() {
  local github_token release_json upload_url
  local -a pkg_assets

  github_token="$(normalize_secret_value "$(fetch_secret CLEANROOM_GITHUB_RELEASE_TOKEN)")"
  [[ -n "${github_token}" ]] || die "CLEANROOM_GITHUB_RELEASE_TOKEN is empty"

  export GITHUB_TOKEN="${github_token}"

  goreleaser release --clean

  release_json="$(
    curl -fsSL \
      -H "Authorization: Bearer ${github_token}" \
      -H "Accept: application/vnd.github+json" \
      -H "X-GitHub-Api-Version: 2022-11-28" \
      "${GITHUB_API_BASE}/releases/tags/${BUILDKITE_TAG}"
  )"
  upload_url="$(
    python3 -c 'import json, sys; print(json.load(sys.stdin)["upload_url"].split("{", 1)[0])' \
      <<<"${release_json}"
  )"
  [[ -n "${upload_url}" ]] || die "failed to resolve GitHub release upload URL for ${BUILDKITE_TAG}"

  pkg_assets=(
    "${RELEASE_EXTRA_DIR}/darwin_arm64/cleanroom_Darwin_arm64.pkg"
    "${RELEASE_EXTRA_DIR}/darwin_arm64/cleanroom_Darwin_arm64.pkg.sha256"
    "${RELEASE_EXTRA_DIR}/darwin_amd64/cleanroom_Darwin_x86_64.pkg"
    "${RELEASE_EXTRA_DIR}/darwin_amd64/cleanroom_Darwin_x86_64.pkg.sha256"
  )

  for asset_path in "${pkg_assets[@]}"; do
    local asset_name asset_id

    [[ -f "${asset_path}" ]] || die "missing release asset: ${asset_path}"
    asset_name="$(basename "${asset_path}")"
    asset_id="$(
      ASSET_NAME="${asset_name}" python3 -c '
import json
import os
import sys

name = os.environ["ASSET_NAME"]
for asset in json.load(sys.stdin).get("assets", []):
    if asset.get("name") == name:
        print(asset.get("id", ""))
        break
' <<<"${release_json}"
    )"
    if [[ -n "${asset_id}" ]]; then
      curl -fsSL -X DELETE \
        -H "Authorization: Bearer ${github_token}" \
        -H "Accept: application/vnd.github+json" \
        -H "X-GitHub-Api-Version: 2022-11-28" \
        "${GITHUB_API_BASE}/releases/assets/${asset_id}" >/dev/null
    fi

    curl -fsSL \
      -X POST \
      -H "Authorization: Bearer ${github_token}" \
      -H "Content-Type: application/octet-stream" \
      -H "Accept: application/vnd.github+json" \
      -H "X-GitHub-Api-Version: 2022-11-28" \
      --data-binary "@${asset_path}" \
      "${upload_url}?name=${asset_name}" >/dev/null
  done
}

require_command buildkite-agent
require_command git
require_command go
require_command tar
require_command curl
require_command python3
require_command goreleaser

[[ -n "${BUILDKITE_TAG:-}" ]] || die "BUILDKITE_TAG is required for release publishing"

cd "${REPO_ROOT}"
git fetch --tags origin

echo "--- :package: Download signed macOS release artifacts"
download_darwin_release_artifacts

echo "--- :hammer: Build Linux release extras"
build_linux_release_extras

echo "--- :rocket: Publish GitHub release"
publish_github_release
