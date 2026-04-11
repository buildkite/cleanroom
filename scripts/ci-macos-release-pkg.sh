#!/usr/bin/env bash
set -euo pipefail

die() {
  printf '[ci-macos-release-pkg] error: %s\n' "$*" >&2
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

resolve_macos_user_name() {
  local console_user

  console_user="$(stat -f '%Su' /dev/console 2>/dev/null || true)"
  if [[ -n "${console_user}" && "${console_user}" != "root" ]]; then
    printf '%s\n' "${console_user}"
    return 0
  fi

  id -un
}

resolve_macos_user_home() {
  local username home_record

  username="$(resolve_macos_user_name)"
  if home_record="$(dscl . -read "/Users/${username}" NFSHomeDirectory 2>/dev/null)" \
    && [[ "${home_record}" == NFSHomeDirectory:* ]]; then
    printf '%s\n' "${home_record#NFSHomeDirectory: }"
    return 0
  fi

  printf '%s\n' "${HOME}"
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
MACOS_USER_HOME="$(resolve_macos_user_home)"

tmpdir=""
keychain_path=""
keychain_password=""
installer_keychain_path=""
installer_keychain_password=""
installer_keychain_cleanup_path=""
helper_profile_path=""
notary_key_path=""
helper_sign_identity=""
helper_sign_selector=""
installer_sign_identity=""
system_keychain_path="/Library/Keychains/System.keychain"
imported_system_helper_identity_hash=""
notary_key_id=""
notary_issuer_id=""

cleanup() {
  if [[ -n "${imported_system_helper_identity_hash}" ]]; then
    sudo security delete-identity -Z "${imported_system_helper_identity_hash}" "${system_keychain_path}" >/dev/null 2>&1 || true
  fi
  if [[ -n "${installer_keychain_cleanup_path}" ]]; then
    rm -f "${installer_keychain_cleanup_path}" >/dev/null 2>&1 || true
  fi
  if [[ -n "${tmpdir}" ]]; then
    rm -rf "${tmpdir}"
  fi
}

trap cleanup EXIT

import_signing_identity_into_system_keychain() {
  local p12_path="$1"
  local p12_password="$2"

  sudo security import "${p12_path}" \
    -k "${system_keychain_path}" \
    -P "${p12_password}" \
    -T /usr/bin/codesign \
    -T /usr/bin/pkgbuild \
    -T /usr/bin/productbuild \
    -T /usr/bin/security >/dev/null
}

assert_codesigning_identity() {
  local common_name="$1"
  local identities

  identities="$(security find-identity -v -p codesigning "${keychain_path}" 2>/dev/null || true)"
  [[ "${identities}" == *"\"${common_name}\""* ]] \
    || die "codesigning identity not found in ${keychain_path}: ${common_name}"
}

resolve_codesigning_identity_selector() {
  local common_name="$1"
  local selector

  selector="$(
    security find-identity -v -p codesigning "${keychain_path}" 2>/dev/null \
      | awk -v name="$common_name" 'index($0, "\"" name "\"") {print $2; exit}'
  )"
  [[ -n "${selector}" ]] || die "unable to resolve codesigning identity selector in ${keychain_path}: ${common_name}"
  printf '%s\n' "${selector}"
}

resolve_certificate_hash() {
  local common_name="$1"
  local hash

  hash="$(
    security find-certificate -a -c "${common_name}" -Z "${keychain_path}" 2>/dev/null \
      | awk '/^SHA-1 hash:/ {print $3; exit}'
  )"
  [[ -n "${hash}" ]] || die "unable to resolve certificate hash in ${keychain_path}: ${common_name}"
  printf '%s\n' "${hash}"
}

assert_certificate_present() {
  local common_name="$1"

  security find-certificate -a -c "${common_name}" "${keychain_path}" >/dev/null 2>&1 \
    || die "certificate not found in ${keychain_path}: ${common_name}"
}

log_codesigning_identities() {
  local identities

  identities="$(security find-identity -v -p codesigning "${keychain_path}" 2>&1 || true)"
  printf '[ci-macos-release-pkg] codesigning identities in %s:\n%s\n' "${keychain_path}" "${identities}" >&2
}

import_installer_signing_certificates() {
  local certificate_url certificate_path

  require_command curl

  for certificate_url in \
    "https://www.apple.com/appleca/AppleIncRootCertificate.cer" \
    "https://www.apple.com/certificateauthority/DeveloperIDCA.cer" \
    "https://www.apple.com/certificateauthority/DeveloperIDG2CA.cer"; do
    certificate_path="${tmpdir}/$(basename "${certificate_url}")"
    curl -fsSL "${certificate_url}" -o "${certificate_path}"
    security import "${certificate_path}" \
      -k "${installer_keychain_path}" \
      -T /usr/bin/pkgbuild \
      -T /usr/bin/productsign \
      -T /usr/bin/productbuild \
      -T /usr/bin/security >/dev/null
  done
}

setup_buildkite_signing_assets() {
  local helper_p12_path
  local helper_p12_password
  local installer_p12_path

  require_command buildkite-agent
  require_command sudo

  helper_p12_path="${tmpdir}/helper-cert.p12"
  installer_p12_path="${tmpdir}/installer-cert.p12"
  helper_profile_path="${tmpdir}/helper.provisionprofile"
  notary_key_path="${tmpdir}/AuthKey.p8"

  printf '%s' "$(fetch_secret CLEANROOM_MACOS_RELEASE_HELPER_CERT_P12_BASE64 | tr -d '\r\n')" \
    | openssl base64 -d -A -out "${helper_p12_path}"
  printf '%s' "$(fetch_secret CLEANROOM_MACOS_RELEASE_INSTALLER_CERT_P12_BASE64 | tr -d '\r\n')" \
    | openssl base64 -d -A -out "${installer_p12_path}"
  printf '%s' "$(fetch_secret CLEANROOM_MACOS_RELEASE_HELPER_PROVISION_PROFILE_BASE64 | tr -d '\r\n')" \
    | openssl base64 -d -A -out "${helper_profile_path}"
  printf '%s' "$(fetch_secret CLEANROOM_MACOS_NOTARY_KEY_P8_BASE64 | tr -d '\r\n')" \
    | openssl base64 -d -A -out "${notary_key_path}"

  helper_p12_password="$(normalize_secret_value "$(fetch_secret CLEANROOM_MACOS_RELEASE_HELPER_CERT_PASSWORD)")"
  installer_keychain_password="$(normalize_secret_value "$(fetch_secret CLEANROOM_MACOS_RELEASE_INSTALLER_CERT_PASSWORD)")"
  helper_sign_identity="$(normalize_secret_value "$(fetch_secret CLEANROOM_MACOS_RELEASE_HELPER_SIGN_IDENTITY)")"
  installer_sign_identity="$(normalize_secret_value "$(fetch_secret CLEANROOM_MACOS_INSTALLER_SIGN_IDENTITY)")"
  notary_key_id="$(normalize_secret_value "$(fetch_secret CLEANROOM_MACOS_NOTARY_KEY_ID)")"
  notary_issuer_id="$(normalize_secret_value "$(fetch_secret CLEANROOM_MACOS_NOTARY_ISSUER_ID)")"
  installer_keychain_path="${MACOS_USER_HOME}/Library/Keychains/cleanroom-installer-signing.keychain-db"

  keychain_path="${system_keychain_path}"
  keychain_password=""
  import_signing_identity_into_system_keychain "${helper_p12_path}" "${helper_p12_password}"

  log_codesigning_identities
  assert_codesigning_identity "${helper_sign_identity}"
  imported_system_helper_identity_hash="$(resolve_certificate_hash "${helper_sign_identity}")"
  helper_sign_selector="$(resolve_codesigning_identity_selector "${helper_sign_identity}")"
  printf '[ci-macos-release-pkg] resolved helper signing selector %s for %s\n' "${helper_sign_selector}" "${helper_sign_identity}" >&2
  installer_keychain_path="${tmpdir}/installer-signing.keychain-db"
  installer_keychain_cleanup_path="${installer_keychain_path}"
  security create-keychain -p "${installer_keychain_password}" "${installer_keychain_path}" >/dev/null
  security unlock-keychain -p "${installer_keychain_password}" "${installer_keychain_path}" >/dev/null
  import_installer_signing_certificates
  security import "${installer_p12_path}" \
    -k "${installer_keychain_path}" \
    -P "${installer_keychain_password}" \
    -T /usr/bin/pkgbuild \
    -T /usr/bin/productsign \
    -T /usr/bin/productbuild \
    -T /usr/bin/security >/dev/null
  security set-key-partition-list \
    -S apple-tool:,apple:,codesign:,pkgbuild:,productsign:,productbuild: \
    -s \
    -k "${installer_keychain_password}" \
    "${installer_keychain_path}" >/dev/null
  security find-certificate -a -c "Developer ID Certification Authority" "${installer_keychain_path}" >/dev/null 2>&1 \
    || die "developer id intermediate not found in ${installer_keychain_path}"
  security find-certificate -a -c "Apple Root CA" "${installer_keychain_path}" >/dev/null 2>&1 \
    || die "apple root ca not found in ${installer_keychain_path}"
  security find-certificate -a -c "${installer_sign_identity}" "${installer_keychain_path}" >/dev/null 2>&1 \
    || die "installer certificate not found in ${installer_keychain_path}: ${installer_sign_identity}"
}

setup_local_signing_assets() {
  helper_profile_path="${CLEANROOM_MACOS_RELEASE_HELPER_PROVISION_PROFILE:-}"
  helper_sign_identity="${CLEANROOM_MACOS_RELEASE_HELPER_SIGN_IDENTITY:-}"
  helper_sign_selector="${CLEANROOM_MACOS_RELEASE_HELPER_SIGN_SELECTOR:-${helper_sign_identity}}"
  installer_sign_identity="${CLEANROOM_MACOS_INSTALLER_SIGN_IDENTITY:-}"
  installer_keychain_path="${CLEANROOM_MACOS_RELEASE_INSTALLER_SIGN_KEYCHAIN:-}"
  installer_keychain_password="${CLEANROOM_MACOS_RELEASE_INSTALLER_SIGN_KEYCHAIN_PASSWORD:-}"
  notary_key_path="${CLEANROOM_MACOS_NOTARY_KEY_PATH:-}"
  notary_key_id="${CLEANROOM_MACOS_NOTARY_KEY_ID:-}"
  notary_issuer_id="${CLEANROOM_MACOS_NOTARY_ISSUER_ID:-}"
  keychain_path="${CLEANROOM_MACOS_RELEASE_HELPER_SIGN_KEYCHAIN:-}"

  [[ -f "${helper_profile_path}" ]] || die "missing helper provisioning profile: ${helper_profile_path}"
  [[ -n "${helper_sign_identity}" ]] || die "CLEANROOM_MACOS_RELEASE_HELPER_SIGN_IDENTITY is required"
  [[ -n "${installer_sign_identity}" ]] || die "CLEANROOM_MACOS_INSTALLER_SIGN_IDENTITY is required"
  [[ -f "${notary_key_path}" ]] || die "missing notary API key: ${notary_key_path}"
  [[ -n "${notary_key_id}" ]] || die "CLEANROOM_MACOS_NOTARY_KEY_ID is required"
  [[ -n "${notary_issuer_id}" ]] || die "CLEANROOM_MACOS_NOTARY_ISSUER_ID is required"
  [[ -n "${keychain_path}" ]] || die "CLEANROOM_MACOS_RELEASE_HELPER_SIGN_KEYCHAIN is required outside Buildkite"
}

resolve_release_metadata() {
  if [[ -n "${CLEANROOM_RELEASE_REF_NAME:-}" && -n "${CLEANROOM_RELEASE_VERSION:-}" ]]; then
    export CLEANROOM_RELEASE_REF_NAME CLEANROOM_RELEASE_VERSION
    return 0
  fi

  if [[ -n "${BUILDKITE_TAG:-}" ]]; then
    CLEANROOM_RELEASE_REF_NAME="${BUILDKITE_TAG}"
    CLEANROOM_RELEASE_VERSION="${BUILDKITE_TAG#v}"
    [[ -n "${CLEANROOM_RELEASE_VERSION}" ]] || die "unable to derive release version from tag ${BUILDKITE_TAG}"
    export CLEANROOM_RELEASE_REF_NAME CLEANROOM_RELEASE_VERSION
    return 0
  fi

  local branch_name short_sha sanitized_branch build_number
  branch_name="${BUILDKITE_BRANCH:-$(git -C "${REPO_ROOT}" rev-parse --abbrev-ref HEAD)}"
  short_sha="$(git -C "${REPO_ROOT}" rev-parse --short=7 HEAD)"
  sanitized_branch="${branch_name//\//-}"
  build_number="${BUILDKITE_BUILD_NUMBER:-0}"

  CLEANROOM_RELEASE_REF_NAME="${sanitized_branch}-${short_sha}"
  CLEANROOM_RELEASE_VERSION="0.0.${build_number}"
  export CLEANROOM_RELEASE_REF_NAME CLEANROOM_RELEASE_VERSION
}

build_release_arch() {
  local arch="$1"
  local asset_arch="$2"
  local goarch="$3"
  local swift_target="$4"
  local release_dir pkg_path

  release_dir="${REPO_ROOT}/release-extra/darwin_${arch}"
  pkg_path="${release_dir}/cleanroom_Darwin_${asset_arch}.pkg"

  mkdir -p "${release_dir}"

  printf '[ci-macos-release-pkg] packaging signed darwin-vz helper for %s\n' "${asset_arch}"
  env \
    CLEANROOM_DARWIN_VZ_HELPER_SWIFT_TARGET="${swift_target}" \
    CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY="${helper_sign_selector}" \
    CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN="${keychain_path}" \
    CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN_PASSWORD="${keychain_password}" \
    CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER:-com.buildkite.cleanroom.darwin-vz}" \
    CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE="${helper_profile_path}" \
    CLEANROOM_DARWIN_VZ_HELPER_SIGN_RUNTIME=1 \
    CLEANROOM_DARWIN_VZ_HELPER_BUNDLE=1 \
      "${SCRIPT_DIR}/build-darwin-vz-helper.sh" "${release_dir}/cleanroom-darwin-vz.app"

  cp "${REPO_ROOT}/cmd/cleanroom-darwin-vz/entitlements.plist" "${release_dir}/entitlements.plist"

  GOOS=darwin GOARCH="${goarch}" CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${CLEANROOM_RELEASE_REF_NAME}" \
    -o "${release_dir}/cleanroom" ./cmd/cleanroom
  GOOS=linux GOARCH="${goarch}" CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w" \
    -o "${release_dir}/cleanroom-guest-agent" ./cmd/cleanroom-guest-agent

  printf '[ci-macos-release-pkg] building and signing release pkg for %s\n' "${asset_arch}"
  env \
    CLEANROOM_MACOS_RELEASE_VERSION="${CLEANROOM_RELEASE_VERSION}" \
    CLEANROOM_MACOS_RELEASE_CLEANROOM_BINARY="${release_dir}/cleanroom" \
    CLEANROOM_MACOS_RELEASE_GUEST_AGENT_BINARY="${release_dir}/cleanroom-guest-agent" \
    CLEANROOM_MACOS_RELEASE_HELPER_BINARY="${release_dir}/cleanroom-darwin-vz.app" \
    CLEANROOM_MACOS_RELEASE_APPLICATION_SIGN_IDENTITY="${helper_sign_selector}" \
    CLEANROOM_MACOS_RELEASE_SIGN_KEYCHAIN="${keychain_path}" \
    CLEANROOM_MACOS_RELEASE_INSTALLER_SIGN_IDENTITY="${installer_sign_identity}" \
    CLEANROOM_MACOS_RELEASE_INSTALLER_SIGN_KEYCHAIN="${installer_keychain_path}" \
    CLEANROOM_MACOS_RELEASE_INSTALLER_SIGN_KEYCHAIN_PASSWORD="${installer_keychain_password}" \
      "${SCRIPT_DIR}/build-macos-release-pkg.sh" "${pkg_path}"

  printf '[ci-macos-release-pkg] notarizing release pkg for %s\n' "${asset_arch}"
  env \
    CLEANROOM_MACOS_NOTARY_KEY_PATH="${notary_key_path}" \
    CLEANROOM_MACOS_NOTARY_KEY_ID="${notary_key_id}" \
    CLEANROOM_MACOS_NOTARY_ISSUER_ID="${notary_issuer_id}" \
      "${SCRIPT_DIR}/notarize-macos-package.sh" "${pkg_path}"

  shasum -a 256 "${pkg_path}" \
    | awk -v name="$(basename "${pkg_path}")" '{print $1 "  " name}' \
    > "${pkg_path}.sha256"
}

upload_buildkite_artifacts() {
  tar -C release-extra -czf release-extra/darwin_arm64.tar.gz darwin_arm64
  tar -C release-extra -czf release-extra/darwin_amd64.tar.gz darwin_amd64
  buildkite-agent artifact upload "release-extra/darwin_*.tar.gz"
  buildkite-agent artifact upload "release-extra/darwin_*/*.pkg"
  buildkite-agent artifact upload "release-extra/darwin_*/*.pkg.sha256"
}

require_command git
require_command go
require_command openssl
require_command security
require_command xcrun
require_command pkgbuild
require_command pkgutil
require_command shasum

tmpdir="$(mktemp -d /tmp/cleanroom-macos-release-pkg.XXXXXX)"

if [[ -n "${BUILDKITE:-}" ]]; then
  setup_buildkite_signing_assets
else
  setup_local_signing_assets
fi

resolve_release_metadata

echo "--- :label: Release metadata"
echo "ref=${CLEANROOM_RELEASE_REF_NAME}"
echo "version=${CLEANROOM_RELEASE_VERSION}"

cd "${REPO_ROOT}"
rm -rf release-extra/darwin_arm64 release-extra/darwin_amd64

echo "--- :apple: Build arm64 notarized pkg"
build_release_arch arm64 arm64 arm64 arm64-apple-macosx13.0

echo "--- :apple: Build x86_64 notarized pkg"
build_release_arch amd64 x86_64 amd64 x86_64-apple-macosx13.0

if [[ -n "${BUILDKITE:-}" ]]; then
  echo "--- :package: Upload Buildkite artifacts"
  upload_buildkite_artifacts
fi
