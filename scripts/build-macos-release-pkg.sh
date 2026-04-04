#!/usr/bin/env bash
set -euo pipefail

die() {
  printf '[build-macos-release-pkg] error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || die "required command not found: ${cmd}"
}

bundle_component_plist=""
helper_cleanup_scripts_dir=""

installer_keychain_default_before=""
declare -a installer_keychain_search_list_before=()
installer_keychain_search_list_changed=0
installer_keychain_default_changed=0

restore_installer_signing_keychain() {
  if [[ "${installer_keychain_default_changed}" == "1" ]]; then
    if [[ -n "${installer_keychain_default_before}" ]]; then
      security default-keychain -d user -s "${installer_keychain_default_before}" >/dev/null 2>&1 || true
    fi
    installer_keychain_default_changed=0
  fi

  if [[ "${installer_keychain_search_list_changed}" == "1" ]]; then
    if ((${#installer_keychain_search_list_before[@]} > 0)); then
      security list-keychains -d user -s "${installer_keychain_search_list_before[@]}" >/dev/null 2>&1 || true
    fi
    installer_keychain_search_list_changed=0
  fi
}

cleanup() {
  restore_installer_signing_keychain
  if [[ -n "${WORK_DIR:-}" ]]; then
    rm -rf "${WORK_DIR}"
  fi
}

prepare_installer_signing_keychain() {
  local listed_keychain default_keychain_output

  [[ -n "${INSTALLER_SIGN_KEYCHAIN}" ]] || return 0

  require_cmd security
  if [[ -n "${INSTALLER_SIGN_KEYCHAIN_PASSWORD}" ]]; then
    security unlock-keychain -p "${INSTALLER_SIGN_KEYCHAIN_PASSWORD}" "${INSTALLER_SIGN_KEYCHAIN}" >/dev/null
  fi

  installer_keychain_search_list_before=()
  while IFS= read -r listed_keychain; do
    listed_keychain="$(printf '%s' "${listed_keychain}" | sed -E 's/^[[:space:]]*"//; s/"$//')"
    if [[ -n "${listed_keychain}" ]]; then
      installer_keychain_search_list_before+=("${listed_keychain}")
    fi
  done < <(security list-keychains -d user 2>/dev/null || true)

  if ((${#installer_keychain_search_list_before[@]} > 0)); then
    security list-keychains -d user -s "${INSTALLER_SIGN_KEYCHAIN}" "${installer_keychain_search_list_before[@]}" >/dev/null
  else
    security list-keychains -d user -s "${INSTALLER_SIGN_KEYCHAIN}" >/dev/null
  fi
  installer_keychain_search_list_changed=1

  if default_keychain_output="$(security default-keychain -d user 2>/dev/null)"; then
    installer_keychain_default_before="$(printf '%s' "${default_keychain_output}" | sed -E 's/^[[:space:]]*"//; s/"$//')"
  else
    installer_keychain_default_before=""
  fi
  security default-keychain -d user -s "${INSTALLER_SIGN_KEYCHAIN}" >/dev/null
  installer_keychain_default_changed=1
}

ensure_bundle_component_plist() {
  [[ -n "${bundle_component_plist}" ]] && return 0

  require_cmd plutil
  bundle_component_plist="${WORK_DIR}/components.plist"
  pkgbuild --analyze --root "${PAYLOAD_ROOT}" "${bundle_component_plist}" >/dev/null
}

configure_bundle_component_plist_entry() {
  local bundle_path="$1"
  local bundle_relative_path component_index component_path

  [[ -d "${bundle_path}" ]] || return 0

  ensure_bundle_component_plist
  bundle_relative_path="${bundle_path#${PAYLOAD_ROOT}/}"

  printf '[build-macos-release-pkg] configuring bundle install behavior for %s\n' "${bundle_relative_path}" >&2

  component_index=""
  for ((component_index=0; ; component_index++)); do
    component_path="$(
      plutil -extract "${component_index}.RootRelativeBundlePath" raw -o - "${bundle_component_plist}" 2>/dev/null || true
    )"
    if [[ -z "${component_path}" ]]; then
      die "unable to find bundle entry in component plist: ${bundle_relative_path}"
    fi
    if [[ "${component_path}" == "${bundle_relative_path}" ]]; then
      break
    fi
  done

  plutil -replace "${component_index}.BundleIsRelocatable" -bool NO "${bundle_component_plist}"
  plutil -replace "${component_index}.BundleHasStrictIdentifier" -bool NO "${bundle_component_plist}"
  plutil -replace "${component_index}.BundleIsVersionChecked" -bool NO "${bundle_component_plist}"
}

configure_bundle_component_plist() {
  configure_bundle_component_plist_entry "${PAYLOAD_HELPER_PATH}"
  configure_bundle_component_plist_entry "${PAYLOAD_SUPPORT_APP_PATH}"
}

configure_helper_cleanup_scripts() {
  local legacy_helper_path

  [[ -d "${PAYLOAD_HELPER_PATH}" ]] || return 0

  helper_cleanup_scripts_dir="${WORK_DIR}/pkg-scripts"
  legacy_helper_path="${INSTALL_PREFIX}/cleanroom-darwin-vz"

  mkdir -p "${helper_cleanup_scripts_dir}"
  cat > "${helper_cleanup_scripts_dir}/postinstall" <<EOF
#!/usr/bin/env bash
set -euo pipefail

target_root="\${3:-/}"
if [[ "\${target_root}" == "/" ]]; then
  target_root=""
fi

legacy_helper_path="\${target_root}${legacy_helper_path}"
if [[ -e "\${legacy_helper_path}" && ! -d "\${legacy_helper_path}" ]]; then
  rm -f "\${legacy_helper_path}"
fi
EOF
  chmod 0755 "${helper_cleanup_scripts_dir}/postinstall"
}

usage() {
  cat <<'USAGE'
Build a macOS installer package for Cleanroom.

Usage:
  build-macos-release-pkg.sh <output.pkg>

Required environment:
  CLEANROOM_MACOS_RELEASE_VERSION                  Package version (for example 0.1.0)
  CLEANROOM_MACOS_RELEASE_CLEANROOM_BINARY        Path to the cleanroom macOS binary
  CLEANROOM_MACOS_RELEASE_GUEST_AGENT_BINARY      Path to the Linux cleanroom-guest-agent binary
  CLEANROOM_MACOS_RELEASE_HELPER_BINARY           Path to the cleanroom-darwin-vz macOS binary or .app bundle
  CLEANROOM_MACOS_RELEASE_SUPPORT_APP             Path to the Cleanroom.app network-filter utility bundle

Optional environment:
  CLEANROOM_MACOS_RELEASE_INSTALL_PREFIX          Install prefix inside the package (default: /usr/local/bin)
  CLEANROOM_MACOS_RELEASE_APPLICATION_SIGN_IDENTITY
                                                  Developer ID Application identity for macOS binaries (default: ad-hoc "-")
  CLEANROOM_MACOS_RELEASE_SIGN_KEYCHAIN          Keychain path to search for signing identities
  CLEANROOM_MACOS_RELEASE_INSTALLER_SIGN_IDENTITY Developer ID Installer identity for the package
USAGE
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

[[ $# -eq 1 ]] || {
  usage >&2
  exit 1
}

OUTPUT_PATH="$1"
VERSION="${CLEANROOM_MACOS_RELEASE_VERSION:-}"
CLEANROOM_BINARY="${CLEANROOM_MACOS_RELEASE_CLEANROOM_BINARY:-}"
GUEST_AGENT_BINARY="${CLEANROOM_MACOS_RELEASE_GUEST_AGENT_BINARY:-}"
HELPER_BINARY="${CLEANROOM_MACOS_RELEASE_HELPER_BINARY:-}"
SUPPORT_APP="${CLEANROOM_MACOS_RELEASE_SUPPORT_APP:-}"
INSTALL_PREFIX="${CLEANROOM_MACOS_RELEASE_INSTALL_PREFIX:-/usr/local/bin}"
APPLICATION_SIGN_IDENTITY="${CLEANROOM_MACOS_RELEASE_APPLICATION_SIGN_IDENTITY:-}"
SIGN_KEYCHAIN="${CLEANROOM_MACOS_RELEASE_SIGN_KEYCHAIN:-}"
INSTALLER_SIGN_IDENTITY="${CLEANROOM_MACOS_RELEASE_INSTALLER_SIGN_IDENTITY:-}"
INSTALLER_SIGN_KEYCHAIN="${CLEANROOM_MACOS_RELEASE_INSTALLER_SIGN_KEYCHAIN:-${SIGN_KEYCHAIN}}"
INSTALLER_SIGN_KEYCHAIN_PASSWORD="${CLEANROOM_MACOS_RELEASE_INSTALLER_SIGN_KEYCHAIN_PASSWORD:-}"
if [[ -z "${APPLICATION_SIGN_IDENTITY}" ]]; then
  APPLICATION_SIGN_IDENTITY="-"
fi

[[ -n "${VERSION}" ]] || die "CLEANROOM_MACOS_RELEASE_VERSION is required"
[[ -f "${CLEANROOM_BINARY}" ]] || die "missing cleanroom binary: ${CLEANROOM_BINARY}"
[[ -f "${GUEST_AGENT_BINARY}" ]] || die "missing cleanroom-guest-agent binary: ${GUEST_AGENT_BINARY}"
[[ -e "${HELPER_BINARY}" ]] || die "missing cleanroom-darwin-vz helper: ${HELPER_BINARY}"
[[ -z "${SUPPORT_APP}" ]] || [[ -d "${SUPPORT_APP}" ]] || die "support app must be a .app bundle: ${SUPPORT_APP}"
[[ "${INSTALL_PREFIX}" = /* ]] || die "install prefix must be absolute: ${INSTALL_PREFIX}"

require_cmd codesign
require_cmd install
require_cmd pkgbuild
require_cmd pkgutil
require_cmd productsign

WORK_DIR="$(mktemp -d)"
trap cleanup EXIT

PAYLOAD_ROOT="${WORK_DIR}/payload"
PAYLOAD_BIN_DIR="${PAYLOAD_ROOT}${INSTALL_PREFIX}"
PAYLOAD_APPLICATIONS_DIR="${PAYLOAD_ROOT}/Applications"
PAYLOAD_CLEANROOM_PATH="${PAYLOAD_BIN_DIR}/cleanroom"
PAYLOAD_GUEST_AGENT_PATH="${PAYLOAD_BIN_DIR}/cleanroom-guest-agent"
PAYLOAD_HELPER_PATH="${PAYLOAD_BIN_DIR}/cleanroom-darwin-vz"
PAYLOAD_SUPPORT_APP_PATH="${PAYLOAD_ROOT}/Applications/Cleanroom.app"
UNSIGNED_OUTPUT_PATH="${WORK_DIR}/unsigned.pkg"

mkdir -p "${PAYLOAD_BIN_DIR}" "$(dirname "${OUTPUT_PATH}")"
install -m 0755 "${CLEANROOM_BINARY}" "${PAYLOAD_CLEANROOM_PATH}"
install -m 0755 "${GUEST_AGENT_BINARY}" "${PAYLOAD_GUEST_AGENT_PATH}"
if [[ -d "${HELPER_BINARY}" ]]; then
  [[ "${HELPER_BINARY}" == *.app ]] || die "helper bundle must end with .app: ${HELPER_BINARY}"
  require_cmd ditto
  PAYLOAD_HELPER_PATH="${PAYLOAD_BIN_DIR}/cleanroom-darwin-vz.app"
  ditto "${HELPER_BINARY}" "${PAYLOAD_HELPER_PATH}"
else
  install -m 0755 "${HELPER_BINARY}" "${PAYLOAD_HELPER_PATH}"
fi
if [[ -n "${SUPPORT_APP}" ]]; then
  require_cmd ditto
  mkdir -p "${PAYLOAD_APPLICATIONS_DIR}"
  ditto "${SUPPORT_APP}" "${PAYLOAD_SUPPORT_APP_PATH}"
fi

codesign_args=(
  --force
  --sign "${APPLICATION_SIGN_IDENTITY}"
)
if [[ -n "${SIGN_KEYCHAIN}" ]]; then
  codesign_args+=(--keychain "${SIGN_KEYCHAIN}")
fi
if [[ "${APPLICATION_SIGN_IDENTITY}" != "-" ]]; then
  codesign_args+=(--options runtime --timestamp)
fi
codesign_args+=("${PAYLOAD_CLEANROOM_PATH}")
printf '[build-macos-release-pkg] signing cleanroom binary\n' >&2
codesign "${codesign_args[@]}"

printf '[build-macos-release-pkg] verifying signed payloads\n' >&2
codesign --verify --strict --verbose=2 "${PAYLOAD_CLEANROOM_PATH}"
codesign --verify --strict --verbose=2 "${PAYLOAD_HELPER_PATH}"
if [[ -d "${PAYLOAD_SUPPORT_APP_PATH}" ]]; then
  codesign --verify --strict --verbose=2 "${PAYLOAD_SUPPORT_APP_PATH}"
fi

configure_bundle_component_plist
configure_helper_cleanup_scripts

pkgbuild_args=(
  --root "${PAYLOAD_ROOT}"
  --identifier "com.buildkite.cleanroom"
  --version "${VERSION}"
  --install-location /
)
if [[ -n "${bundle_component_plist}" ]]; then
  pkgbuild_args+=(--component-plist "${bundle_component_plist}")
fi
if [[ -n "${helper_cleanup_scripts_dir}" ]]; then
  pkgbuild_args+=(--scripts "${helper_cleanup_scripts_dir}")
fi
if [[ -n "${INSTALLER_SIGN_IDENTITY}" ]]; then
  pkgbuild_args+=("${UNSIGNED_OUTPUT_PATH}")
else
  pkgbuild_args+=("${OUTPUT_PATH}")
fi
printf '[build-macos-release-pkg] building installer package\n' >&2
pkgbuild "${pkgbuild_args[@]}"
if [[ -n "${INSTALLER_SIGN_IDENTITY}" ]]; then
  prepare_installer_signing_keychain
  productsign_args=(
    --sign "${INSTALLER_SIGN_IDENTITY}"
  )
  if [[ -n "${INSTALLER_SIGN_KEYCHAIN}" ]]; then
    productsign_args+=(--keychain "${INSTALLER_SIGN_KEYCHAIN}")
  fi
  productsign_args+=("${UNSIGNED_OUTPUT_PATH}" "${OUTPUT_PATH}")
  printf '[build-macos-release-pkg] signing installer package\n' >&2
  productsign "${productsign_args[@]}"
  pkgutil --check-signature "${OUTPUT_PATH}"
fi
