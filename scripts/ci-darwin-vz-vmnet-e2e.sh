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

  while IFS= read -r keychain; do
    [[ -n "$keychain" ]] || continue
    original_keychains+=("$keychain")
  done < <(security list-keychains -d user | sed -E 's/^[[:space:]]*"//; s/"$//')
  if default_keychain_output="$(security default-keychain -d user 2>/dev/null)"; then
    original_default_keychain="$(printf '%s' "$default_keychain_output" | sed -E 's/^[[:space:]]*"//; s/"$//')"
  else
    original_default_keychain=""
  fi

  security create-keychain -p "$temp_keychain_password" "$temp_keychain_path" >/dev/null
  security unlock-keychain -p "$temp_keychain_password" "$temp_keychain_path" >/dev/null
  security set-keychain-settings -lut 21600 "$temp_keychain_path" >/dev/null
  security import "$p12_path" -k "$temp_keychain_path" -P "$p12_password" -T /usr/bin/codesign -T /usr/bin/security
  security import "$wwdr_path" -k "$temp_keychain_path" -T /usr/bin/codesign -T /usr/bin/security
  security list-keychains -d user -s \
    "$temp_keychain_path" \
    "$HOME/Library/Keychains/login.keychain-db" \
    /Library/Keychains/System.keychain \
    /System/Library/Keychains/SystemRootCertificates.keychain >/dev/null
  security default-keychain -d user -s "$temp_keychain_path" >/dev/null
  security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$temp_keychain_password" "$temp_keychain_path" >/dev/null

  if ! security find-certificate -a -c "$requested_sign_identity" "$temp_keychain_path" >/dev/null 2>&1; then
    echo "imported signing certificate not found in temp keychain: $requested_sign_identity" >&2
    security find-certificate -a -Z "$temp_keychain_path" >&2 || true
    exit 1
  fi

  local imported_sign_identity
  imported_sign_identity="$(
    security find-certificate -a -c "$requested_sign_identity" -Z "$temp_keychain_path" 2>/dev/null \
      | awk '/^SHA-1 hash:/ {print $3; exit}'
  )"
  if [[ -z "$imported_sign_identity" ]]; then
    echo "unable to derive imported signing certificate hash from temp keychain" >&2
    security find-certificate -a -Z "$temp_keychain_path" >&2 || true
    exit 1
  fi

  local available_identities
  # `security find-identity <keychain>` is not reliable for temp keychains on all
  # macOS images; query the configured user search list instead.
  available_identities="$(security find-identity -v -p codesigning 2>&1 || true)"
  if ! grep -F -- "\"$requested_sign_identity\"" <<<"$available_identities" >/dev/null; then
    echo "warning: imported signing identity not found in configured keychain search list: $requested_sign_identity" >&2
    printf '%s\n' "$available_identities" >&2
    echo "warning: continuing to codesign with the temp keychain; codesign will be the source of truth" >&2
  fi

  sign_identity="$imported_sign_identity"
  # On Buildkite macOS agents, codesign sees the imported identity more
  # reliably via the configured default/search-list keychains than via
  # an explicit --keychain override.
  sign_keychain=""
}

setup_local_signing_assets() {
  profile_path="${CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE:-}"
  sign_identity="$(normalize_secret_value "${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY:-}")"
  sign_keychain="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN:-}"
}

restore_keychains() {
  if [[ -n "${original_default_keychain:-}" ]]; then
    security default-keychain -d user -s "$original_default_keychain" >/dev/null 2>&1 || true
  fi
  if ((${#original_keychains[@]} > 0)); then
    security list-keychains -d user -s "${original_keychains[@]}" >/dev/null 2>&1 || true
  fi
}

cleanup() {
  restore_keychains
  if [[ -n "${temp_keychain_path:-}" ]]; then
    security delete-keychain "$temp_keychain_path" >/dev/null 2>&1 || true
  fi
  rm -rf "${tmpdir:-}"
}

require_command security
require_command curl
require_command openssl
require_command codesign

tmpdir="$(mktemp -d /tmp/cleanroom-dvz-vmnet-e2e.XXXXXX)"
declare -a original_keychains=()
trap cleanup EXIT

temp_keychain_path="$tmpdir/cleanroom-signing.keychain-db"
temp_keychain_password="$(openssl rand -hex 16)"

p12_path="$tmpdir/helper-cert.p12"
profile_path="$tmpdir/helper.provisionprofile"
wwdr_path="$tmpdir/AppleWWDRCAG3.cer"
sign_identity=""
sign_keychain=""
p12_password=""
helper_path="$(resolve_local_helper_path)"

if [[ -z "${BUILDKITE:-}" ]]; then
  setup_local_signing_assets
else
  setup_buildkite_signing_assets
fi

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
codesign --verify --strict --verbose=2 "$helper_path"

echo "--- :apple: VMNet E2E"
CLEANROOM_DARWIN_VZ_HELPER="$helper_path" \
CLEANROOM_DARWIN_VZ_VMNET_E2E=1 \
go test ./internal/backend/darwinvz -run TestVMNetSharedE2E -v
