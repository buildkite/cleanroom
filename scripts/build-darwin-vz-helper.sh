#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

OUTPUT_PATH="${1:-${REPO_ROOT}/dist/cleanroom-darwin-vz.app}"
SWIFT_TARGET="${CLEANROOM_DARWIN_VZ_HELPER_SWIFT_TARGET:-}"
ENTITLEMENTS_PATH="${CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS:-${REPO_ROOT}/cmd/cleanroom-darwin-vz/entitlements.plist}"
PROVISION_PROFILE="${CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE:-}"
BUNDLE_MODE="${CLEANROOM_DARWIN_VZ_HELPER_BUNDLE:-1}"

[[ -f "${REPO_ROOT}/cmd/cleanroom-darwin-vz/main.swift" ]] || {
  echo "missing helper source: ${REPO_ROOT}/cmd/cleanroom-darwin-vz/main.swift" >&2
  exit 1
}
[[ -f "${ENTITLEMENTS_PATH}" ]] || {
  echo "missing entitlements plist: ${ENTITLEMENTS_PATH}" >&2
  exit 1
}
if [[ -n "${PROVISION_PROFILE}" && ! -f "${PROVISION_PROFILE}" ]]; then
  echo "missing provisioning profile: ${PROVISION_PROFILE}" >&2
  exit 1
fi

mkdir -p "$(dirname "${OUTPUT_PATH}")"
tmpdir="$(mktemp -d /tmp/cleanroom-darwin-vz-build.XXXXXX)"
trap 'rm -rf "${tmpdir}"' EXIT
build_output_path="${tmpdir}/cleanroom-darwin-vz"

swiftc_args=(
  -O
  -framework Virtualization
  -framework vmnet
)
if [[ -n "${SWIFT_TARGET}" ]]; then
  swiftc_args+=(-target "${SWIFT_TARGET}")
fi
swiftc_args+=(
  "${REPO_ROOT}/cmd/cleanroom-darwin-vz/main.swift"
  -o "${build_output_path}"
)

xcrun swiftc "${swiftc_args[@]}"

package_env=(
  "CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS=${ENTITLEMENTS_PATH}"
  "CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY=${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY:-}"
  "CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN=${CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN:-}"
  "CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER=${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER:-}"
  "CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE=${PROVISION_PROFILE}"
)
if [[ -n "${PROVISION_PROFILE}" || "${BUNDLE_MODE}" != "0" ]]; then
  package_env+=("CLEANROOM_DARWIN_VZ_HELPER_BUNDLE=1")
fi

env "${package_env[@]}" "${SCRIPT_DIR}/package-darwin-vz-helper.sh" "${build_output_path}" "${OUTPUT_PATH}"
