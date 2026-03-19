#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

require_command() {
  local name="$1"
  command -v "$name" >/dev/null 2>&1 || {
    echo "missing required command: $name" >&2
    exit 1
  }
}

fetch_secret() {
  local key="$1"
  buildkite-agent secret get "$key"
}

normalize_secret_value() {
  printf '%s' "$1" | tr -d '\r'
}

resolve_macos_user_home() {
  local username home_record

  username="$(resolve_macos_user_name)"
  if home_record="$(dscl . -read "/Users/${username}" NFSHomeDirectory 2>/dev/null)" \
    && [[ "$home_record" == NFSHomeDirectory:* ]]; then
    printf '%s\n' "${home_record#NFSHomeDirectory: }"
    return 0
  fi

  printf '%s\n' "$HOME"
}

resolve_macos_user_name() {
  local console_user

  console_user="$(stat -f '%Su' /dev/console 2>/dev/null || true)"
  if [[ -n "$console_user" && "$console_user" != "root" ]]; then
    printf '%s\n' "$console_user"
    return 0
  fi

  id -un
}

run_with_macos_user_home() {
  if [[ -n "${macos_user_home:-}" ]]; then
    HOME="$macos_user_home" USER="$macos_user_name" LOGNAME="$macos_user_name" "$@"
    return 0
  fi

  "$@"
}

resolve_macos_provisioning_udid() {
  system_profiler SPHardwareDataType 2>/dev/null | awk -F': ' '/Provisioning UDID/ {print $2; exit}'
}

assert_profile_allows_current_device() {
  [[ -n "${profile_path:-}" && -f "$profile_path" ]] || return 0

  local provisioning_udid
  provisioning_udid="$(resolve_macos_provisioning_udid)"
  [[ -n "$provisioning_udid" ]] || return 0

  openssl smime -inform der -verify -noverify -in "$profile_path" -out "$decoded_profile_path" >/dev/null
  if ! grep -F "<string>${provisioning_udid}</string>" "$decoded_profile_path" >/dev/null 2>&1; then
    echo "provisioning profile does not allow this Mac's Provisioning UDID: $provisioning_udid" >&2
    echo "regenerate the vmnet development provisioning profile with this device added and update the Buildkite secret" >&2
    exit 1
  fi
}

resolve_local_helper_path() {
  local helper="${CLEANROOM_DARWIN_VZ_HELPER:-}"
  if [[ -n "$helper" ]]; then
    printf '%s\n' "$helper"
    return 0
  fi
  printf '%s\n' "$REPO_ROOT/dist/cleanroom-darwin-vz.app"
}

setup_buildkite_signing_assets() {
  require_command buildkite-agent

  printf '%s' "$(fetch_secret CLEANROOM_DARWIN_VZ_HELPER_CERT_P12_BASE64 | tr -d '\r\n')" | openssl base64 -d -A -out "$p12_path"
  printf '%s' "$(fetch_secret CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE_BASE64 | tr -d '\r\n')" | openssl base64 -d -A -out "$profile_path"
  local requested_sign_identity
  requested_sign_identity="$(normalize_secret_value "$(fetch_secret CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY)")"
  p12_password="$(normalize_secret_value "$(fetch_secret CLEANROOM_DARWIN_VZ_HELPER_CERT_PASSWORD)")"
  curl -fsSL https://www.apple.com/certificateauthority/AppleWWDRCAG3.cer -o "$wwdr_path"
  wwdr_fingerprint="$(openssl x509 -inform der -in "$wwdr_path" -noout -fingerprint -sha1 | awk -F= '{gsub(":", "", $2); print $2}')"

  if ! sudo security import "$p12_path" -k "$system_keychain_path" -P "$p12_password" -T /usr/bin/codesign -T /usr/bin/security >/dev/null; then
    echo "failed to import signing identity into ${system_keychain_path}" >&2
    exit 1
  fi

  if security find-certificate -a -Z "$system_keychain_path" 2>/dev/null | awk '/^SHA-1 hash:/ {print $3}' | grep -Fx -- "$wwdr_fingerprint" >/dev/null; then
    wwdr_added_to_system_keychain=0
  else
    sudo security add-certificates -k "$system_keychain_path" "$wwdr_path" >/dev/null
    wwdr_added_to_system_keychain=1
  fi

  local imported_sign_identity
  imported_sign_identity="$(
    security find-certificate -a -c "$requested_sign_identity" -Z "$system_keychain_path" 2>/dev/null \
      | awk '/^SHA-1 hash:/ {print $3; exit}'
  )"
  if [[ -z "$imported_sign_identity" ]]; then
    echo "unable to derive imported signing certificate hash from ${system_keychain_path}" >&2
    security find-certificate -a -Z "$system_keychain_path" >&2 || true
    exit 1
  fi

  local available_identities
  available_identities="$(security find-identity -v -p codesigning "$system_keychain_path" 2>&1 || true)"
  if ! grep -F -- "\"$requested_sign_identity\"" <<<"$available_identities" >/dev/null; then
    echo "imported signing identity not found in ${system_keychain_path}: $requested_sign_identity" >&2
    printf '%s\n' "$available_identities" >&2
    exit 1
  fi

  imported_system_identity_hash="$imported_sign_identity"
  sign_identity="$requested_sign_identity"
  sign_keychain="$system_keychain_path"
}

setup_local_signing_assets() {
  profile_path="${CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE:-}"
  sign_identity="$(normalize_secret_value "${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY:-}")"
  sign_keychain="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN:-}"
}

cleanup() {
  if [[ -n "${imported_system_identity_hash:-}" ]]; then
    sudo security delete-identity -Z "$imported_system_identity_hash" "$system_keychain_path" >/dev/null 2>&1 || true
  fi
  if [[ "${wwdr_added_to_system_keychain:-0}" == "1" && -n "${wwdr_fingerprint:-}" ]]; then
    sudo security delete-certificate -Z "$wwdr_fingerprint" "$system_keychain_path" >/dev/null 2>&1 || true
  fi
  rm -rf "${tmpdir:-}"
}

require_command security
require_command curl
require_command openssl
require_command codesign
require_command sudo

tmpdir="$(mktemp -d /tmp/cleanroom-dvz-vmnet-e2e.XXXXXX)"
trap cleanup EXIT

macos_user_name="$(resolve_macos_user_name)"
macos_user_home="$(resolve_macos_user_home)"

p12_path="$tmpdir/helper-cert.p12"
profile_path="$tmpdir/helper.provisionprofile"
decoded_profile_path="$tmpdir/helper.provisionprofile.plist"
wwdr_path="$tmpdir/AppleWWDRCAG3.cer"
wwdr_fingerprint=""
wwdr_added_to_system_keychain=0
system_keychain_path="/Library/Keychains/System.keychain"
imported_system_identity_hash=""
sign_identity=""
sign_keychain=""
p12_password=""
helper_path="$(resolve_local_helper_path)"

if [[ -z "${BUILDKITE:-}" ]]; then
  setup_local_signing_assets
else
  setup_buildkite_signing_assets
fi
assert_profile_allows_current_device

echo "--- :hammer: Building binaries"
cd "$REPO_ROOT"
scripts/build-go.sh

if [[ ! -d "$helper_path" ]]; then
  [[ -n "$profile_path" ]] || {
    echo "vmnet helper bundle is missing: $helper_path" >&2
    echo "Set CLEANROOM_DARWIN_VZ_HELPER to a prebuilt signed helper bundle or provide CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE and CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY." >&2
    exit 1
  }
  [[ -n "$sign_identity" ]] || {
    echo "CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY is required to build a local vmnet helper" >&2
    exit 1
  }

  echo "--- :key: Building vmnet-signed helper"
  run_with_macos_user_home env \
    CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS=cmd/cleanroom-darwin-vz/entitlements-vmnet.plist \
    CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY="$sign_identity" \
    CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN="$sign_keychain" \
    CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER:-com.buildkite.cleanroom.darwin-vz}" \
    CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE="$profile_path" \
    CLEANROOM_DARWIN_VZ_HELPER_BUNDLE=1 \
    scripts/build-darwin-vz-helper.sh dist/cleanroom-darwin-vz
fi

if [[ ! -d "$helper_path" ]]; then
  echo "darwin-vz helper bundle is missing: $helper_path" >&2
  exit 1
fi
run_with_macos_user_home codesign --verify --strict --verbose=2 "$helper_path"

echo "--- :apple: VMNet E2E"
CLEANROOM_DARWIN_VZ_HELPER="$helper_path" \
CLEANROOM_DARWIN_VZ_VMNET_E2E=1 \
go test ./internal/backend/darwinvz -run TestVMNetSharedE2E -v
