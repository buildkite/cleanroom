#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

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
  install.sh [--version <version>] [--prefix <prefix>] [--repo <owner/repo>] [--no-darwin-helper]

Examples:
  curl -fsSL https://raw.githubusercontent.com/buildkite/cleanroom/main/scripts/install.sh | bash
  curl -fsSL https://raw.githubusercontent.com/buildkite/cleanroom/main/scripts/install.sh | \
    bash -s -- --version vX.Y.Z --prefix "$HOME/.local"

Environment variables:
  CLEANROOM_VERSION               Optional release version (example: vX.Y.Z)
  CLEANROOM_PREFIX                Install prefix (default: ~/.local for non-root, /usr/local for root)
  CLEANROOM_REPO                  GitHub repo in owner/repo format (default: buildkite/cleanroom)
  CLEANROOM_INSTALL_DARWIN_HELPER Set to 0 to skip cleanroom-darwin-vz install on macOS
  CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS Optional entitlements plist for cleanroom-darwin-vz signing or re-signing
  CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY Optional codesign identity for cleanroom-darwin-vz (default: ad-hoc when re-signing)
  CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER Optional codesign identifier for cleanroom-darwin-vz (default: com.buildkite.cleanroom.darwin-vz when embedding a provisioning profile)
  CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE Optional provisioning profile to embed when building or re-signing a helper bundle
  CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN Optional keychain path when using the repo helper packager

Deprecated compatibility:
  CLEANROOM_INSTALL_DIR           Legacy alias for a bin dir such as /usr/local/bin; inferred into CLEANROOM_PREFIX
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

default_prefix() {
  if [ "$(id -u)" -eq 0 ]; then
    printf '/usr/local'
    return
  fi
  printf '%s/.local' "$HOME"
}

infer_prefix_from_install_dir() {
  local dir="$1"
  case "$dir" in
    */bin) printf '%s' "${dir%/bin}" ;;
    *) die "legacy install dir must end with /bin: ${dir}" ;;
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

extract_archive() {
  local archive="$1"
  local output_dir="$2"
  mkdir -p "$output_dir"
  tar -xzf "$archive" -C "$output_dir"
}

declare -a SUDO_CMD=()

prepare_prefix_dirs() {
  for dir in "$BIN_DIR" "$LIBEXEC_DIR"; do
    if [ ! -d "$dir" ]; then
      if [ "$(id -u)" -eq 0 ]; then
        mkdir -p "$dir"
      else
        if mkdir -p "$dir" 2>/dev/null; then
          :
        else
          command -v sudo >/dev/null 2>&1 || die "${dir} does not exist and sudo is unavailable"
          SUDO_CMD=(sudo)
          "${SUDO_CMD[@]}" mkdir -p "$dir"
        fi
      fi
    fi
  done

  if [ "$(id -u)" -ne 0 ] && { [ ! -w "$BIN_DIR" ] || [ ! -w "$LIBEXEC_DIR" ]; }; then
    command -v sudo >/dev/null 2>&1 || die "${PREFIX} is not writable and sudo is unavailable"
    SUDO_CMD=(sudo)
  fi
}

install_binary() {
  local src="$1"
  local dst="$2"
  "${SUDO_CMD[@]}" install -m 0755 "$src" "$dst"
}

install_file() {
  local src="$1"
  local dst="$2"
  "${SUDO_CMD[@]}" install -m 0644 "$src" "$dst"
}

install_app_bundle() {
  local src="$1"
  local dst="$2"
  "${SUDO_CMD[@]}" rm -rf "$dst"
  if [ "${#SUDO_CMD[@]}" -gt 0 ]; then
    "${SUDO_CMD[@]}" ditto "$src" "$dst"
    return
  fi
  ditto "$src" "$dst"
}

package_darwin_helper_with_repo_script() {
  local src="$1"
  local dst="$2"
  local package_script="${SCRIPT_DIR}/package-darwin-vz-helper.sh"
  local -a cmd

  [ -f "$package_script" ] || return 1

  cmd=("${SUDO_CMD[@]}" env)
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

resolve_archive_path() {
  local staged="$1"
  local legacy="$2"
  if [ -e "$staged" ]; then
    printf '%s' "$staged"
    return
  fi
  if [ -e "$legacy" ]; then
    printf '%s' "$legacy"
    return
  fi
  printf '%s' "$staged"
}

HOST_OS_RAW="$(uname -s)"
HOST_ARCH_RAW="$(uname -m)"

case "$HOST_OS_RAW" in
  Linux) HOST_OS="Linux" ;;
  Darwin) HOST_OS="Darwin" ;;
  *) die "unsupported operating system: ${HOST_OS_RAW}" ;;
esac

case "$HOST_ARCH_RAW" in
  x86_64|amd64) HOST_ARCH="x86_64"; HOST_GOARCH="amd64" ;;
  arm64|aarch64) HOST_ARCH="arm64"; HOST_GOARCH="arm64" ;;
  *) die "unsupported architecture: ${HOST_ARCH_RAW}" ;;
esac

VERSION="${CLEANROOM_VERSION:-}"
PREFIX="${CLEANROOM_PREFIX:-}"
if [ -z "$PREFIX" ] && [ -n "${CLEANROOM_INSTALL_DIR:-}" ]; then
  PREFIX="$(infer_prefix_from_install_dir "$CLEANROOM_INSTALL_DIR")"
fi
PREFIX="${PREFIX:-$(default_prefix)}"
REPO="${CLEANROOM_REPO:-buildkite/cleanroom}"
INSTALL_DARWIN_HELPER="${CLEANROOM_INSTALL_DARWIN_HELPER:-1}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      [ "$#" -ge 2 ] || die "--version requires a value"
      VERSION="$2"
      shift 2
      ;;
    --prefix)
      [ "$#" -ge 2 ] || die "--prefix requires a value"
      PREFIX="$2"
      shift 2
      ;;
    --install-dir)
      [ "$#" -ge 2 ] || die "--install-dir requires a value"
      PREFIX="$(infer_prefix_from_install_dir "$2")"
      warn "--install-dir is deprecated; use --prefix instead"
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

PREFIX="${PREFIX%/}"
BIN_DIR="${PREFIX}/bin"
LIBEXEC_DIR="${PREFIX}/libexec/cleanroom"

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

log "Installing cleanroom from ${REPO} (${RELEASE_LABEL}) for ${HOST_OS}/${HOST_ARCH} into ${PREFIX}"

CHECKSUMS_PATH="${WORK_DIR}/checksums.txt"
download "${RELEASE_BASE}/checksums.txt" "$CHECKSUMS_PATH"

CLEANROOM_ASSET="cleanroom_${HOST_OS}_${HOST_ARCH}.tar.gz"
CLEANROOM_ARCHIVE_PATH="${WORK_DIR}/${CLEANROOM_ASSET}"

download "${RELEASE_BASE}/${CLEANROOM_ASSET}" "$CLEANROOM_ARCHIVE_PATH"
verify_asset_against_checksums "$CLEANROOM_ASSET" "$CLEANROOM_ARCHIVE_PATH" "$CHECKSUMS_PATH"

CLEANROOM_EXTRACT_DIR="${WORK_DIR}/cleanroom"
extract_archive "$CLEANROOM_ARCHIVE_PATH" "$CLEANROOM_EXTRACT_DIR"

CLEANROOM_BIN_SRC="$(resolve_archive_path "${CLEANROOM_EXTRACT_DIR}/bin/cleanroom" "${CLEANROOM_EXTRACT_DIR}/cleanroom")"
GUEST_AGENT_LINUX_SRC="$(resolve_archive_path "${CLEANROOM_EXTRACT_DIR}/libexec/cleanroom/cleanroom-guest-agent-linux-${HOST_GOARCH}" "${CLEANROOM_EXTRACT_DIR}/cleanroom-guest-agent")"

[ -f "${CLEANROOM_BIN_SRC}" ] || die "cleanroom binary missing in ${CLEANROOM_ASSET}"
[ -f "${GUEST_AGENT_LINUX_SRC}" ] || die "cleanroom-guest-agent-linux-${HOST_GOARCH} missing in ${CLEANROOM_ASSET}"

prepare_prefix_dirs
install_binary "${CLEANROOM_BIN_SRC}" "${BIN_DIR}/cleanroom"
install_binary "${GUEST_AGENT_LINUX_SRC}" "${LIBEXEC_DIR}/cleanroom-guest-agent-linux-${HOST_GOARCH}"

if [ "$HOST_OS" = "Darwin" ] && [ "$INSTALL_DARWIN_HELPER" != "0" ]; then
  HELPER_BUNDLE_SRC="$(resolve_archive_path "${CLEANROOM_EXTRACT_DIR}/libexec/cleanroom/cleanroom-darwin-vz.app" "${CLEANROOM_EXTRACT_DIR}/cleanroom-darwin-vz.app")"
  HELPER_BINARY_SRC="$(resolve_archive_path "${CLEANROOM_EXTRACT_DIR}/libexec/cleanroom/cleanroom-darwin-vz" "${CLEANROOM_EXTRACT_DIR}/cleanroom-darwin-vz")"
  HELPER_ENTITLEMENTS_VMNET_PATH="$(resolve_archive_path "${CLEANROOM_EXTRACT_DIR}/libexec/cleanroom/entitlements-vmnet.plist" "${CLEANROOM_EXTRACT_DIR}/entitlements-vmnet.plist")"
  HELPER_ENTITLEMENTS_DEFAULT_PATH="$(resolve_archive_path "${CLEANROOM_EXTRACT_DIR}/libexec/cleanroom/entitlements.plist" "${CLEANROOM_EXTRACT_DIR}/entitlements.plist")"
  HELPER_SIGN_IDENTITY="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY:-}"
  HELPER_SIGN_IDENTIFIER="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER:-}"
  HELPER_SIGN_KEYCHAIN="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN:-}"
  HELPER_PROVISION_PROFILE="${CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE:-}"
  HELPER_BUNDLE_EMBEDDED_PROFILE_PATH="${HELPER_BUNDLE_SRC}/Contents/embedded.provisionprofile"
  HELPER_RESIGN_REQUESTED=0
  if [ "${CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS+x}" = "x" ] || \
     [ "${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY+x}" = "x" ] || \
     [ "${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER+x}" = "x" ] || \
     [ "${CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE+x}" = "x" ]; then
    HELPER_RESIGN_REQUESTED=1
  fi

  HELPER_ENTITLEMENTS_PATH="${CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS:-}"
  if [ -z "${HELPER_ENTITLEMENTS_PATH}" ]; then
    if [ -f "${HELPER_ENTITLEMENTS_VMNET_PATH}" ] && { \
      [ -n "${HELPER_PROVISION_PROFILE}" ] || \
      { [ "${HELPER_RESIGN_REQUESTED}" = "0" ] && [ -f "${HELPER_BUNDLE_EMBEDDED_PROFILE_PATH}" ]; }; \
    }; then
      HELPER_ENTITLEMENTS_PATH="${HELPER_ENTITLEMENTS_VMNET_PATH}"
    else
      HELPER_ENTITLEMENTS_PATH="${HELPER_ENTITLEMENTS_DEFAULT_PATH}"
    fi
  fi

  if [ -n "${HELPER_PROVISION_PROFILE}" ] && [ ! -f "${HELPER_PROVISION_PROFILE}" ]; then
    die "provisioning profile missing: ${HELPER_PROVISION_PROFILE}"
  fi
  if [ -n "${HELPER_PROVISION_PROFILE}" ] && [ -z "${HELPER_SIGN_IDENTIFIER}" ]; then
    HELPER_SIGN_IDENTIFIER="com.buildkite.cleanroom.darwin-vz"
  fi

  if [ -d "${HELPER_BUNDLE_SRC}" ]; then
    require_cmd ditto
    HELPER_BUNDLE_DIR="${LIBEXEC_DIR}/cleanroom-darwin-vz.app"
    install_app_bundle "${HELPER_BUNDLE_SRC}" "${HELPER_BUNDLE_DIR}"
    HELPER_INSTALL_LOG_PATH="${HELPER_BUNDLE_DIR}"

    if [ "${HELPER_RESIGN_REQUESTED}" = "1" ]; then
      HELPER_SIGN_IDENTITY="${HELPER_SIGN_IDENTITY:--}"
      if ! package_darwin_helper_with_repo_script "${HELPER_BUNDLE_DIR}" "${HELPER_BUNDLE_DIR}"; then
        require_cmd codesign
        [ -f "${HELPER_ENTITLEMENTS_PATH}" ] || die "entitlements plist missing: ${HELPER_ENTITLEMENTS_PATH}"
        HELPER_BUNDLE_PROFILE_DEST="${HELPER_BUNDLE_DIR}/Contents/embedded.provisionprofile"
        if [ -n "${HELPER_PROVISION_PROFILE}" ]; then
          install_file "${HELPER_PROVISION_PROFILE}" "${HELPER_BUNDLE_PROFILE_DEST}"
        else
          "${SUDO_CMD[@]}" rm -f "${HELPER_BUNDLE_PROFILE_DEST}"
        fi
        codesign_cmd=("${SUDO_CMD[@]}" codesign --force --sign "${HELPER_SIGN_IDENTITY}" --entitlements "${HELPER_ENTITLEMENTS_PATH}")
        if [ -n "${HELPER_SIGN_KEYCHAIN}" ]; then
          codesign_cmd+=(--keychain "${HELPER_SIGN_KEYCHAIN}")
        fi
        if [ -n "${HELPER_SIGN_IDENTIFIER}" ]; then
          codesign_cmd+=(-i "${HELPER_SIGN_IDENTIFIER}")
        fi
        codesign_cmd+=("${HELPER_BUNDLE_DIR}")
        "${codesign_cmd[@]}"
      fi
    fi

    if [ -f "${HELPER_ENTITLEMENTS_DEFAULT_PATH}" ]; then
      install_file "${HELPER_ENTITLEMENTS_DEFAULT_PATH}" "${LIBEXEC_DIR}/entitlements.plist"
    fi
    if [ -f "${HELPER_ENTITLEMENTS_VMNET_PATH}" ]; then
      install_file "${HELPER_ENTITLEMENTS_VMNET_PATH}" "${LIBEXEC_DIR}/entitlements-vmnet.plist"
    fi
    DARWIN_HELPER_INSTALLED=1
  else
    [ -f "${HELPER_BINARY_SRC}" ] || die "cleanroom-darwin-vz missing in ${CLEANROOM_ASSET}"

    HELPER_SIGN_IDENTITY="${HELPER_SIGN_IDENTITY:--}"
    if [ -n "${HELPER_PROVISION_PROFILE}" ]; then
      HELPER_SIGN_TARGET="${LIBEXEC_DIR}/cleanroom-darwin-vz.app"
    else
      HELPER_SIGN_TARGET="${LIBEXEC_DIR}/cleanroom-darwin-vz"
    fi
    HELPER_INSTALL_LOG_PATH="${HELPER_SIGN_TARGET}"
    if ! package_darwin_helper_with_repo_script "${HELPER_BINARY_SRC}" "${HELPER_SIGN_TARGET}"; then
      require_cmd codesign
      [ -f "${HELPER_ENTITLEMENTS_PATH}" ] || die "entitlements plist missing: ${HELPER_ENTITLEMENTS_PATH}"
      if [ -n "${HELPER_PROVISION_PROFILE}" ]; then
        HELPER_BUNDLE_DIR="${LIBEXEC_DIR}/cleanroom-darwin-vz.app"
        HELPER_EXECUTABLE_PATH="${HELPER_BUNDLE_DIR}/Contents/MacOS/cleanroom-darwin-vz"
        HELPER_INFO_PLIST_PATH="${HELPER_BUNDLE_DIR}/Contents/Info.plist"
        "${SUDO_CMD[@]}" mkdir -p "$(dirname "${HELPER_EXECUTABLE_PATH}")"
        "${SUDO_CMD[@]}" sh -c "cat > \"${HELPER_INFO_PLIST_PATH}\" <<'EOF'
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
        install_file "${HELPER_PROVISION_PROFILE}" "${HELPER_BUNDLE_DIR}/Contents/embedded.provisionprofile"
        HELPER_SIGN_TARGET="${HELPER_BUNDLE_DIR}"
        HELPER_INSTALL_LOG_PATH="${HELPER_BUNDLE_DIR}"
      else
        install_binary "${HELPER_BINARY_SRC}" "${LIBEXEC_DIR}/cleanroom-darwin-vz"
      fi
      codesign_cmd=("${SUDO_CMD[@]}" codesign --force --sign "${HELPER_SIGN_IDENTITY}" --entitlements "${HELPER_ENTITLEMENTS_PATH}")
      if [ -n "${HELPER_SIGN_KEYCHAIN}" ]; then
        codesign_cmd+=(--keychain "${HELPER_SIGN_KEYCHAIN}")
      fi
      if [ -n "${HELPER_SIGN_IDENTIFIER}" ]; then
        codesign_cmd+=(-i "${HELPER_SIGN_IDENTIFIER}")
      fi
      codesign_cmd+=("${HELPER_SIGN_TARGET}")
      "${codesign_cmd[@]}"
    fi
    DARWIN_HELPER_INSTALLED=1
  fi
fi

log "Installed cleanroom to ${BIN_DIR}/cleanroom"
log "Installed cleanroom runtime assets to ${LIBEXEC_DIR}"
if [ "$DARWIN_HELPER_INSTALLED" = "1" ]; then
  log "Installed cleanroom-darwin-vz to ${HELPER_INSTALL_LOG_PATH}"
fi

case ":${PATH}:" in
  *":${BIN_DIR}:"*) ;;
  *) warn "${BIN_DIR} is not in PATH" ;;
esac
