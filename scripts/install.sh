#!/usr/bin/env bash
set -euo pipefail

SCRIPT_SOURCE="${BASH_SOURCE[0]-}"
SCRIPT_DIR=""
if [ -n "${SCRIPT_SOURCE}" ] && [ -f "${SCRIPT_SOURCE}" ]; then
  SCRIPT_DIR="$(cd "$(dirname "${SCRIPT_SOURCE}")" && pwd)"
fi

log() {
  printf '[cleanroom-install] %s\n' "$*"
}

warn() {
  printf '[cleanroom-install] warning: %s\n' "$*" >&2
}

die() {
  printf '[cleanroom-install] error: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'USAGE'
Install cleanroom from GitHub releases.

Usage:
  install.sh [--version <version>] [--install-dir <dir>] [--repo <owner/repo>] [--no-darwin-helper]

Examples:
  curl -fsSL https://raw.githubusercontent.com/buildkite/cleanroom/main/scripts/install.sh | bash
  curl -fsSL https://raw.githubusercontent.com/buildkite/cleanroom/main/scripts/install.sh | \
    bash -s -- --version vX.Y.Z

Environment variables:
  CLEANROOM_VERSION               Optional release version (example: vX.Y.Z)
  CLEANROOM_INSTALL_DIR           Install destination (default: /usr/local/bin)
  CLEANROOM_REPO                  GitHub repo in owner/repo format (default: buildkite/cleanroom)
  CLEANROOM_INSTALL_DARWIN_HELPER Set to 0 to skip cleanroom-darwin-vz install on macOS
  CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS Optional entitlements plist for cleanroom-darwin-vz signing or re-signing
  CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY Optional codesign identity for cleanroom-darwin-vz (default: ad-hoc when re-signing)
  CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER Optional codesign identifier for cleanroom-darwin-vz (default: com.buildkite.cleanroom.darwin-vz when embedding a provisioning profile)
  CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE Optional provisioning profile to embed when building or re-signing a helper bundle
  CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN Optional keychain path when using the repo helper packager
USAGE
}

require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || die "required command not found: ${cmd}"
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

download() {
  local url="$1"
  local dest="$2"
  if ! curl -fsSL --retry 3 --connect-timeout 10 "$url" -o "$dest"; then
    die "failed to download ${url}"
  fi
}

download_if_exists() {
  local url="$1"
  local dest="$2"
  curl -fsSL --retry 3 --connect-timeout 10 "$url" -o "$dest"
}

normalize_version() {
  local raw="$1"
  if [ -z "$raw" ] || [ "$raw" = "latest" ]; then
    printf 'latest'
    return
  fi

  case "$raw" in
    v*) printf '%s' "$raw" ;;
    *) printf 'v%s' "$raw" ;;
  esac
}

lookup_checksum() {
  local asset="$1"
  local checksums_file="$2"
  local checksum

  checksum=$(awk -v name="$asset" '$2 == name { print $1 }' "$checksums_file")
  if [ -z "$checksum" ]; then
    die "checksum for ${asset} not found in ${checksums_file}"
  fi

  printf '%s' "$checksum"
}

verify_asset_against_checksums() {
  local asset="$1"
  local asset_path="$2"
  local checksums_file="$3"
  local expected actual

  expected=$(lookup_checksum "$asset" "$checksums_file")
  actual=$(sha256_file "$asset_path")

  if [ "$expected" != "$actual" ]; then
    die "checksum mismatch for ${asset}"
  fi
}

verify_asset_against_checksum_file() {
  local asset="$1"
  local asset_path="$2"
  local checksum_file="$3"
  local expected actual

  expected=$(awk -v name="$asset" '$2 == name { print $1 }' "$checksum_file")
  if [ -z "$expected" ]; then
    die "checksum for ${asset} not found in ${checksum_file}"
  fi

  actual=$(sha256_file "$asset_path")
  if [ "$expected" != "$actual" ]; then
    die "checksum mismatch for ${asset}"
  fi
}

extract_binary() {
  local archive="$1"
  local output_dir="$2"
  mkdir -p "$output_dir"
  tar -xzf "$archive" -C "$output_dir"
}

restore_flattened_darwin_helper_bundle() {
  local extract_dir="$1"
  local bundle_dir="${extract_dir}/cleanroom-darwin-vz.app"
  local bundle_contents_dir="${bundle_dir}/Contents"
  local executable_path="${extract_dir}/cleanroom-darwin-vz"
  local info_plist_path="${extract_dir}/Info.plist"
  local code_resources_path="${extract_dir}/CodeResources"
  local embedded_profile_path="${extract_dir}/embedded.provisionprofile"

  [ -d "${bundle_dir}" ] && return 0
  [ -f "${executable_path}" ] || return 0
  [ -f "${info_plist_path}" ] || return 0

  mkdir -p "${bundle_contents_dir}/MacOS"
  mv "${executable_path}" "${bundle_contents_dir}/MacOS/cleanroom-darwin-vz"
  mv "${info_plist_path}" "${bundle_contents_dir}/Info.plist"

  if [ -f "${code_resources_path}" ]; then
    mkdir -p "${bundle_contents_dir}/_CodeSignature"
    mv "${code_resources_path}" "${bundle_contents_dir}/_CodeSignature/CodeResources"
  fi

  if [ -f "${embedded_profile_path}" ]; then
    mv "${embedded_profile_path}" "${bundle_contents_dir}/embedded.provisionprofile"
  fi
}

declare -a SUDO_CMD=()

run_with_optional_sudo() {
  if [ "${#SUDO_CMD[@]}" -gt 0 ]; then
    "${SUDO_CMD[@]}" "$@"
    return
  fi
  "$@"
}

prepare_install_dir() {
  if [ ! -d "$INSTALL_DIR" ]; then
    if [ "$(id -u)" -eq 0 ]; then
      mkdir -p "$INSTALL_DIR"
    else
      if mkdir -p "$INSTALL_DIR" 2>/dev/null; then
        :
      else
        command -v sudo >/dev/null 2>&1 || die "${INSTALL_DIR} does not exist and sudo is unavailable"
        SUDO_CMD=(sudo)
        "${SUDO_CMD[@]}" mkdir -p "$INSTALL_DIR"
      fi
    fi
  fi

  if [ "$(id -u)" -ne 0 ] && [ ! -w "$INSTALL_DIR" ]; then
    command -v sudo >/dev/null 2>&1 || die "${INSTALL_DIR} is not writable and sudo is unavailable"
    SUDO_CMD=(sudo)
  fi
}

install_binary() {
  local src="$1"
  local dst="$2"
  run_with_optional_sudo install -m 0755 "$src" "$dst"
}

install_app_bundle() {
  local src="$1"
  local dst="$2"
  run_with_optional_sudo rm -rf "$dst"
  run_with_optional_sudo ditto "$src" "$dst"
}

install_macos_pkg() {
  local pkg_path="$1"

  require_cmd installer
  if [ "$(id -u)" -ne 0 ]; then
    command -v sudo >/dev/null 2>&1 || die "sudo is required to install ${pkg_path}"
    SUDO_CMD=(sudo)
  fi
  run_with_optional_sudo installer -pkg "$pkg_path" -target /
}

homebrew_e2fsprogs_prefixes() {
  local brew_path prefix

  if brew_path="$(command -v brew 2>/dev/null)"; then
    prefix="$("${brew_path}" --prefix e2fsprogs 2>/dev/null || true)"
    if [ -n "${prefix}" ]; then
      printf '%s\n' "${prefix}"
    fi
  fi

  printf '%s\n' /opt/homebrew/opt/e2fsprogs /usr/local/opt/e2fsprogs
}

resolve_homebrew_e2fsprogs_binary() {
  local binary="$1"
  local path prefix subdir

  if path="$(command -v "${binary}" 2>/dev/null)"; then
    printf '%s\n' "${path}"
    return 0
  fi

  while IFS= read -r prefix; do
    [ -n "${prefix}" ] || continue
    for subdir in sbin bin; do
      path="${prefix}/${subdir}/${binary}"
      if [ -x "${path}" ]; then
        printf '%s\n' "${path}"
        return 0
      fi
    done
  done < <(homebrew_e2fsprogs_prefixes)

  return 1
}

prompt_install_homebrew_package() {
  local package="$1"
  local answer

  [ -r /dev/tty ] && [ -w /dev/tty ] || return 1
  printf '[cleanroom-install] Install %s with Homebrew now? [y/N] ' "${package}" >/dev/tty
  IFS= read -r answer </dev/tty || return 1

  case "${answer}" in
    y|Y|yes|YES|Yes) return 0 ;;
    *) return 1 ;;
  esac
}

maybe_install_darwin_host_dependencies() {
  local brew_path
  local -a missing=()

  [ "${HOST_OS}" = "Darwin" ] || return 0

  if ! resolve_homebrew_e2fsprogs_binary mkfs.ext4 >/dev/null; then
    missing+=(mkfs.ext4)
  fi
  if ! resolve_homebrew_e2fsprogs_binary debugfs >/dev/null; then
    missing+=(debugfs)
  fi
  if [ "${#missing[@]}" -eq 0 ]; then
    return 0
  fi

  warn "macOS host dependencies missing: ${missing[*]}; install them with \`brew install e2fsprogs\`"
  if ! brew_path="$(command -v brew 2>/dev/null)"; then
    warn "Homebrew was not found; install Homebrew or run \`brew install e2fsprogs\` after installing it"
    return 0
  fi

  if ! prompt_install_homebrew_package e2fsprogs; then
    warn "skipping e2fsprogs install; run \`brew install e2fsprogs\`, then \`cleanroom doctor\`"
    return 0
  fi

  log "Installing e2fsprogs with Homebrew"
  if ! "${brew_path}" install e2fsprogs; then
    warn "failed to install e2fsprogs with Homebrew; run \`brew install e2fsprogs\`, then \`cleanroom doctor\`"
    return 0
  fi

  if resolve_homebrew_e2fsprogs_binary mkfs.ext4 >/dev/null && resolve_homebrew_e2fsprogs_binary debugfs >/dev/null; then
    log "Installed e2fsprogs host tools"
  else
    warn "e2fsprogs install finished, but mkfs.ext4/debugfs were not found; run \`cleanroom doctor\` for details"
  fi
}

can_use_notarized_macos_pkg() {
  [ "$HOST_OS" = "Darwin" ] || return 1
  [ "$INSTALL_DIR" = "/usr/local/bin" ] || return 1
  [ "$INSTALL_DARWIN_HELPER" != "0" ] || return 1

  for key in \
    CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS \
    CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY \
    CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER \
    CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE \
    CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN; do
    if [ "${!key+x}" = "x" ]; then
      return 1
    fi
  done

  return 0
}

try_install_notarized_macos_pkg() {
  local pkg_asset pkg_path pkg_checksum_path

  pkg_asset="cleanroom_${HOST_OS}_${HOST_ARCH}.pkg"
  pkg_path="${WORK_DIR}/${pkg_asset}"
  pkg_checksum_path="${pkg_path}.sha256"

  if ! download_if_exists "${RELEASE_BASE}/${pkg_asset}" "${pkg_path}"; then
    warn "no notarized macOS pkg found for ${RELEASE_LABEL}; falling back to archive install"
    return 1
  fi
  if ! download_if_exists "${RELEASE_BASE}/${pkg_asset}.sha256" "${pkg_checksum_path}"; then
    warn "no pkg checksum found for ${RELEASE_LABEL}; falling back to archive install"
    return 1
  fi

  verify_asset_against_checksum_file "${pkg_asset}" "${pkg_path}" "${pkg_checksum_path}"
  if ! install_macos_pkg "${pkg_path}"; then
    warn "failed to install notarized macOS package: ${pkg_asset}; falling back to archive install"
    return 1
  fi

  log "Installed cleanroom via notarized macOS package"
  log "Installed cleanroom to ${INSTALL_DIR}/cleanroom"
  log "Installed cleanroom-guest-agent to ${INSTALL_DIR}/cleanroom-guest-agent"
  log "Installed cleanroom-darwin-vz to ${INSTALL_DIR}/cleanroom-darwin-vz.app"
  return 0
}

package_darwin_helper_with_repo_script() {
  local src="$1"
  local dst="$2"
  local package_script
  local -a cmd

  [ -n "${SCRIPT_DIR}" ] || return 1
  package_script="${SCRIPT_DIR}/package-darwin-vz-helper.sh"
  [ -f "$package_script" ] || return 1

  if [ "${#SUDO_CMD[@]}" -gt 0 ]; then
    cmd=("${SUDO_CMD[@]}" env)
  else
    cmd=(env)
  fi
  cmd+=("CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS=${HELPER_ENTITLEMENTS_PATH}")
  cmd+=("CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY=${HELPER_SIGN_IDENTITY}")
  cmd+=("CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER=${HELPER_SIGN_IDENTIFIER}")
  if [ -n "${HELPER_SIGN_KEYCHAIN:-}" ]; then
    cmd+=("CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN=${HELPER_SIGN_KEYCHAIN}")
  fi
  if [ -n "${HELPER_PROVISION_PROFILE:-}" ]; then
    cmd+=("CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE=${HELPER_PROVISION_PROFILE}")
  fi
  cmd+=("$package_script" "$src" "$dst")
  "${cmd[@]}"
}

HOST_OS_RAW="$(uname -s)"
HOST_ARCH_RAW="$(uname -m)"

case "$HOST_OS_RAW" in
  Linux) HOST_OS="Linux" ;;
  Darwin) HOST_OS="Darwin" ;;
  *) die "unsupported operating system: ${HOST_OS_RAW}" ;;
esac

case "$HOST_ARCH_RAW" in
  x86_64|amd64) HOST_ARCH="x86_64" ;;
  arm64|aarch64) HOST_ARCH="arm64" ;;
  *) die "unsupported architecture: ${HOST_ARCH_RAW}" ;;
esac

VERSION="${CLEANROOM_VERSION:-}"
INSTALL_DIR="${CLEANROOM_INSTALL_DIR:-/usr/local/bin}"
REPO="${CLEANROOM_REPO:-buildkite/cleanroom}"
INSTALL_DARWIN_HELPER="${CLEANROOM_INSTALL_DARWIN_HELPER:-1}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      [ "$#" -ge 2 ] || die "--version requires a value"
      VERSION="$2"
      shift 2
      ;;
    --install-dir)
      [ "$#" -ge 2 ] || die "--install-dir requires a value"
      INSTALL_DIR="$2"
      shift 2
      ;;
    --repo)
      [ "$#" -ge 2 ] || die "--repo requires a value"
      REPO="$2"
      shift 2
      ;;
    --no-darwin-helper)
      INSTALL_DARWIN_HELPER=0
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1"
      ;;
  esac
done

require_cmd curl
require_cmd tar
require_cmd awk

VERSION="$(normalize_version "$VERSION")"
if [ "$VERSION" = "latest" ]; then
  RELEASE_BASE="https://github.com/${REPO}/releases/latest/download"
  RELEASE_LABEL="latest"
else
  RELEASE_BASE="https://github.com/${REPO}/releases/download/${VERSION}"
  RELEASE_LABEL="$VERSION"
fi

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT
DARWIN_HELPER_INSTALLED=0

log "Installing cleanroom from ${REPO} (${RELEASE_LABEL}) for ${HOST_OS}/${HOST_ARCH}"

if can_use_notarized_macos_pkg && try_install_notarized_macos_pkg; then
  case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *) warn "${INSTALL_DIR} is not in PATH" ;;
  esac
  maybe_install_darwin_host_dependencies
  exit 0
fi

CHECKSUMS_PATH="${WORK_DIR}/checksums.txt"
download "${RELEASE_BASE}/checksums.txt" "$CHECKSUMS_PATH"

CLEANROOM_ASSET="cleanroom_${HOST_OS}_${HOST_ARCH}.tar.gz"
CLEANROOM_ARCHIVE_PATH="${WORK_DIR}/${CLEANROOM_ASSET}"

download "${RELEASE_BASE}/${CLEANROOM_ASSET}" "$CLEANROOM_ARCHIVE_PATH"
verify_asset_against_checksums "$CLEANROOM_ASSET" "$CLEANROOM_ARCHIVE_PATH" "$CHECKSUMS_PATH"

CLEANROOM_EXTRACT_DIR="${WORK_DIR}/cleanroom"
extract_binary "$CLEANROOM_ARCHIVE_PATH" "$CLEANROOM_EXTRACT_DIR"
if [ "$HOST_OS" = "Darwin" ]; then
  restore_flattened_darwin_helper_bundle "$CLEANROOM_EXTRACT_DIR"
fi
[ -f "${CLEANROOM_EXTRACT_DIR}/cleanroom" ] || die "cleanroom binary missing in ${CLEANROOM_ASSET}"
[ -f "${CLEANROOM_EXTRACT_DIR}/cleanroom-guest-agent" ] || die "cleanroom-guest-agent missing in ${CLEANROOM_ASSET}"

prepare_install_dir
install_binary "${CLEANROOM_EXTRACT_DIR}/cleanroom" "${INSTALL_DIR}/cleanroom"
install_binary "${CLEANROOM_EXTRACT_DIR}/cleanroom-guest-agent" "${INSTALL_DIR}/cleanroom-guest-agent"

if [ "$HOST_OS" = "Darwin" ] && [ "$INSTALL_DARWIN_HELPER" != "0" ]; then
  HELPER_BUNDLE_SRC="${CLEANROOM_EXTRACT_DIR}/cleanroom-darwin-vz.app"
  HELPER_BINARY_SRC="${CLEANROOM_EXTRACT_DIR}/cleanroom-darwin-vz"
  HELPER_ENTITLEMENTS_DEFAULT_PATH="${CLEANROOM_EXTRACT_DIR}/entitlements.plist"
  HELPER_SIGN_IDENTITY="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY:-}"
  HELPER_SIGN_IDENTIFIER="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER:-}"
  HELPER_SIGN_KEYCHAIN="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN:-}"
  HELPER_PROVISION_PROFILE="${CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE:-}"
  HELPER_RESIGN_REQUESTED=0
  if [ "${CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS+x}" = "x" ] || \
     [ "${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY+x}" = "x" ] || \
     [ "${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER+x}" = "x" ] || \
     [ "${CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE+x}" = "x" ]; then
    HELPER_RESIGN_REQUESTED=1
  fi

  HELPER_ENTITLEMENTS_PATH="${CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS:-}"
  if [ -z "${HELPER_ENTITLEMENTS_PATH}" ]; then
    HELPER_ENTITLEMENTS_PATH="${HELPER_ENTITLEMENTS_DEFAULT_PATH}"
  fi

  if [ -n "${HELPER_PROVISION_PROFILE}" ] && [ ! -f "${HELPER_PROVISION_PROFILE}" ]; then
    die "provisioning profile missing: ${HELPER_PROVISION_PROFILE}"
  fi
  if [ -n "${HELPER_PROVISION_PROFILE}" ] && [ -z "${HELPER_SIGN_IDENTIFIER}" ]; then
    HELPER_SIGN_IDENTIFIER="com.buildkite.cleanroom.darwin-vz"
  fi

  if [ -d "${HELPER_BUNDLE_SRC}" ]; then
    require_cmd ditto
    HELPER_BUNDLE_DIR="${INSTALL_DIR}/cleanroom-darwin-vz.app"
    install_app_bundle "${HELPER_BUNDLE_SRC}" "${HELPER_BUNDLE_DIR}"
    HELPER_INSTALL_LOG_PATH="${HELPER_BUNDLE_DIR}"

    if [ "${HELPER_RESIGN_REQUESTED}" = "1" ]; then
      HELPER_SIGN_IDENTITY="${HELPER_SIGN_IDENTITY:--}"
      if ! package_darwin_helper_with_repo_script "${HELPER_BUNDLE_DIR}" "${HELPER_BUNDLE_DIR}"; then
        require_cmd codesign
        [ -f "${HELPER_ENTITLEMENTS_PATH}" ] || die "entitlements plist missing: ${HELPER_ENTITLEMENTS_PATH}"
        HELPER_BUNDLE_PROFILE_DEST="${HELPER_BUNDLE_DIR}/Contents/embedded.provisionprofile"
        if [ -n "${HELPER_PROVISION_PROFILE}" ]; then
          install_binary "${HELPER_PROVISION_PROFILE}" "${HELPER_BUNDLE_PROFILE_DEST}"
        else
          run_with_optional_sudo rm -f "${HELPER_BUNDLE_PROFILE_DEST}"
        fi
        codesign_cmd=(codesign --force --sign "${HELPER_SIGN_IDENTITY}" --entitlements "${HELPER_ENTITLEMENTS_PATH}")
        if [ -n "${HELPER_SIGN_KEYCHAIN}" ]; then
          codesign_cmd+=(--keychain "${HELPER_SIGN_KEYCHAIN}")
        fi
        if [ -n "${HELPER_SIGN_IDENTIFIER}" ]; then
          codesign_cmd+=(-i "${HELPER_SIGN_IDENTIFIER}")
        fi
        codesign_cmd+=("${HELPER_BUNDLE_DIR}")
        if [ "${#SUDO_CMD[@]}" -gt 0 ]; then
          codesign_cmd=("${SUDO_CMD[@]}" "${codesign_cmd[@]}")
        fi
        "${codesign_cmd[@]}"
      fi
    fi

    DARWIN_HELPER_INSTALLED=1
  else
    [ -f "${HELPER_BINARY_SRC}" ] || die "cleanroom-darwin-vz missing in ${CLEANROOM_ASSET}"

    HELPER_SIGN_IDENTITY="${HELPER_SIGN_IDENTITY:--}"
    if [ -n "${HELPER_PROVISION_PROFILE}" ]; then
      HELPER_SIGN_TARGET="${INSTALL_DIR}/cleanroom-darwin-vz.app"
    else
      HELPER_SIGN_TARGET="${INSTALL_DIR}/cleanroom-darwin-vz"
    fi
    HELPER_INSTALL_LOG_PATH="${HELPER_SIGN_TARGET}"
    if ! package_darwin_helper_with_repo_script "${HELPER_BINARY_SRC}" "${HELPER_SIGN_TARGET}"; then
      require_cmd codesign
      [ -f "${HELPER_ENTITLEMENTS_PATH}" ] || die "entitlements plist missing: ${HELPER_ENTITLEMENTS_PATH}"
      if [ -n "${HELPER_PROVISION_PROFILE}" ]; then
        HELPER_BUNDLE_DIR="${INSTALL_DIR}/cleanroom-darwin-vz.app"
        HELPER_EXECUTABLE_PATH="${HELPER_BUNDLE_DIR}/Contents/MacOS/cleanroom-darwin-vz"
        HELPER_INFO_PLIST_PATH="${HELPER_BUNDLE_DIR}/Contents/Info.plist"
        run_with_optional_sudo mkdir -p "$(dirname "${HELPER_EXECUTABLE_PATH}")"
        run_with_optional_sudo sh -c "cat > \"${HELPER_INFO_PLIST_PATH}\" <<'EOF'
<?xml version=\"1.0\" encoding=\"UTF-8\"?>
<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">
<plist version=\"1.0\">
<dict>
  <key>CFBundleExecutable</key>
  <string>cleanroom-darwin-vz</string>
  <key>CFBundleIdentifier</key>
  <string>${HELPER_SIGN_IDENTIFIER}</string>
  <key>CFBundleName</key>
  <string>cleanroom-darwin-vz</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>CFBundleShortVersionString</key>
  <string>0.0.0</string>
  <key>CFBundleVersion</key>
  <string>1</string>
</dict>
</plist>
EOF"
        install_binary "${HELPER_BINARY_SRC}" "${HELPER_EXECUTABLE_PATH}"
        install_binary "${HELPER_PROVISION_PROFILE}" "${HELPER_BUNDLE_DIR}/Contents/embedded.provisionprofile"
        HELPER_SIGN_TARGET="${HELPER_BUNDLE_DIR}"
        HELPER_INSTALL_LOG_PATH="${HELPER_BUNDLE_DIR}"
      else
        install_binary "${HELPER_BINARY_SRC}" "${INSTALL_DIR}/cleanroom-darwin-vz"
      fi
      codesign_cmd=(codesign --force --sign "${HELPER_SIGN_IDENTITY}" --entitlements "${HELPER_ENTITLEMENTS_PATH}")
      if [ -n "${HELPER_SIGN_KEYCHAIN}" ]; then
        codesign_cmd+=(--keychain "${HELPER_SIGN_KEYCHAIN}")
      fi
      if [ -n "${HELPER_SIGN_IDENTIFIER}" ]; then
        codesign_cmd+=(-i "${HELPER_SIGN_IDENTIFIER}")
      fi
      codesign_cmd+=("${HELPER_SIGN_TARGET}")
      if [ "${#SUDO_CMD[@]}" -gt 0 ]; then
        codesign_cmd=("${SUDO_CMD[@]}" "${codesign_cmd[@]}")
      fi
      "${codesign_cmd[@]}"
    fi
    DARWIN_HELPER_INSTALLED=1
  fi
fi

log "Installed cleanroom to ${INSTALL_DIR}/cleanroom"
log "Installed cleanroom-guest-agent to ${INSTALL_DIR}/cleanroom-guest-agent"
if [ "$DARWIN_HELPER_INSTALLED" = "1" ]; then
  log "Installed cleanroom-darwin-vz to ${HELPER_INSTALL_LOG_PATH}"
fi

case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *) warn "${INSTALL_DIR} is not in PATH" ;;
esac

maybe_install_darwin_host_dependencies
