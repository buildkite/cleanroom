#!/usr/bin/env bash
set -euo pipefail

die() {
  printf '[build-darwin-vz-minimal-kernel-release] error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  local name="$1"
  command -v "$name" >/dev/null 2>&1 || die "missing required command: ${name}"
}

sha256_file() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
    return
  fi
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print $1}'
    return
  fi
  die "sha256 tool not found (need sha256sum or shasum)"
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

KERNEL_VERSION="${CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_VERSION:-6.1.155}"
KERNEL_PROFILE="${CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_PROFILE:-rootfs}"
KERNEL_ARCH="${CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_ARCH:-arm64}"
DOCKER_IMAGE="${CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_DOCKER_IMAGE:-ubuntu:22.04}"
DOCKER_PLATFORM="${CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_DOCKER_PLATFORM:-linux/${KERNEL_ARCH}}"
KERNEL_TARBALL_SHA256="${CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_TARBALL_SHA256:-}"
if [[ -z "${KERNEL_TARBALL_SHA256}" && "${KERNEL_VERSION}" == "6.1.155" ]]; then
  KERNEL_TARBALL_SHA256="c29387aeee085fbcbd91236224b9df805063bac43615e75cea2c6b29604a5c73"
fi

case "${KERNEL_PROFILE}" in
  rootfs) ;;
  *)
    die "release kernel profile must be rootfs, got ${KERNEL_PROFILE}"
    ;;
esac

case "${KERNEL_ARCH}" in
  arm64) ;;
  *)
    die "release kernel arch must be arm64, got ${KERNEL_ARCH}"
    ;;
esac

require_command docker
require_command git
require_command python3

OUTPUT_DIR="${1:-${REPO_ROOT}/release-extra/kernels}"
ASSET_BASE="${CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_ASSET_BASE:-cleanroom-darwin-vz-minimal-${KERNEL_PROFILE}-${KERNEL_ARCH}-linux-${KERNEL_VERSION}}"
IMAGE_NAME="${ASSET_BASE}-Image"
IMAGE_PATH="${OUTPUT_DIR}/${IMAGE_NAME}"
CONFIG_PATH="${IMAGE_PATH}.config"
SHA256_PATH="${IMAGE_PATH}.sha256"
MANIFEST_PATH="${ASSET_BASE}.manifest.json"
MANIFEST_PATH="${OUTPUT_DIR}/${MANIFEST_PATH}"

mkdir -p "${OUTPUT_DIR}"
rm -f "${IMAGE_PATH}" "${CONFIG_PATH}" "${SHA256_PATH}" "${MANIFEST_PATH}"

CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_PROFILE="${KERNEL_PROFILE}" \
CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_ARCH="${KERNEL_ARCH}" \
CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_VERSION="${KERNEL_VERSION}" \
CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_DOCKER_IMAGE="${DOCKER_IMAGE}" \
CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_DOCKER_PLATFORM="${DOCKER_PLATFORM}" \
CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_TARBALL_SHA256="${KERNEL_TARBALL_SHA256}" \
  "${REPO_ROOT}/benchmarks/darwin-vz/minimal/build-kernel.sh" "${IMAGE_PATH}" >/dev/null

[[ -f "${IMAGE_PATH}" ]] || die "kernel image was not created: ${IMAGE_PATH}"
[[ -f "${CONFIG_PATH}" ]] || die "kernel config was not created: ${CONFIG_PATH}"

KERNEL_SHA256="$(sha256_file "${IMAGE_PATH}")"
printf '%s  %s\n' "${KERNEL_SHA256}" "${IMAGE_NAME}" > "${SHA256_PATH}"

SOURCE_COMMIT="$(git -C "${REPO_ROOT}" rev-parse HEAD)"
SOURCE_REPOSITORY="${CLEANROOM_GITHUB_REPOSITORY:-buildkite/cleanroom}"
RELEASE_TAG="${BUILDKITE_TAG:-}"

ASSET_BASE="${ASSET_BASE}" \
DOCKER_IMAGE="${DOCKER_IMAGE}" \
DOCKER_PLATFORM="${DOCKER_PLATFORM}" \
IMAGE_NAME="${IMAGE_NAME}" \
KERNEL_ARCH="${KERNEL_ARCH}" \
KERNEL_PROFILE="${KERNEL_PROFILE}" \
KERNEL_SHA256="${KERNEL_SHA256}" \
KERNEL_TARBALL_SHA256="${KERNEL_TARBALL_SHA256}" \
KERNEL_VERSION="${KERNEL_VERSION}" \
RELEASE_TAG="${RELEASE_TAG}" \
SOURCE_COMMIT="${SOURCE_COMMIT}" \
SOURCE_REPOSITORY="${SOURCE_REPOSITORY}" \
python3 - <<'PY' > "${MANIFEST_PATH}"
import json
import os

image_name = os.environ["IMAGE_NAME"]
manifest = {
    "id": os.environ["ASSET_BASE"],
    "backend": "darwin-vz",
    "profile": os.environ["KERNEL_PROFILE"],
    "arch": os.environ["KERNEL_ARCH"],
    "linux_version": os.environ["KERNEL_VERSION"],
    "assets": {
        "image": image_name,
        "config": image_name + ".config",
        "sha256": image_name + ".sha256",
        "manifest": os.environ["ASSET_BASE"] + ".manifest.json",
    },
    "sha256": os.environ["KERNEL_SHA256"],
    "source": {
        "repository": os.environ["SOURCE_REPOSITORY"],
        "commit": os.environ["SOURCE_COMMIT"],
        "tag": os.environ["RELEASE_TAG"],
    },
    "builder": {
        "script": "benchmarks/darwin-vz/minimal/build-kernel.sh",
        "docker_image": os.environ["DOCKER_IMAGE"],
        "docker_platform": os.environ["DOCKER_PLATFORM"],
        "kernel_tarball_sha256": os.environ["KERNEL_TARBALL_SHA256"],
    },
}
print(json.dumps(manifest, indent=2, sort_keys=True))
PY

printf '[build-darwin-vz-minimal-kernel-release] wrote %s\n' "${IMAGE_PATH}"
printf '[build-darwin-vz-minimal-kernel-release] wrote %s\n' "${CONFIG_PATH}"
printf '[build-darwin-vz-minimal-kernel-release] wrote %s\n' "${SHA256_PATH}"
printf '[build-darwin-vz-minimal-kernel-release] wrote %s\n' "${MANIFEST_PATH}"
