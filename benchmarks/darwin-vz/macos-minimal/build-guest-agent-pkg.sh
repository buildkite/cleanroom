#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
OUT="${1:-"${ROOT_DIR}/dist/cleanroom-macos-guest-agent.pkg"}"
AGENT_BIN="${CLEANROOM_MACOS_GUEST_AGENT_BIN:-"${ROOT_DIR}/dist/cleanroom-macos-guest-agent"}"
AGENT_VERSION="${CLEANROOM_MACOS_GUEST_AGENT_VERSION:-0.1.0}"
AGENT_PORT="${CLEANROOM_MACOS_GUEST_AGENT_PORT:-10700}"
LABEL="com.buildkite.cleanroom.macos-guest-agent"

case "${AGENT_PORT}" in
  ''|*[!0-9]*)
    echo "build-guest-agent-pkg: CLEANROOM_MACOS_GUEST_AGENT_PORT must be numeric" >&2
    exit 1
    ;;
esac

if [[ ! -x "${AGENT_BIN}" ]]; then
  "${ROOT_DIR}/benchmarks/darwin-vz/macos-minimal/build-guest-agent.sh" "${AGENT_BIN}" >/dev/null
fi

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/cleanroom-macos-agent-pkg.XXXXXX")"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

SCRIPTS="${TMP_DIR}/scripts"
PLIST="${SCRIPTS}/${LABEL}.plist"

mkdir -p \
  "${SCRIPTS}" \
  "$(dirname "${OUT}")"

install -m 0755 "${AGENT_BIN}" "${SCRIPTS}/cleanroom-macos-guest-agent"
sed "s/<string>10700<\\/string>/<string>${AGENT_PORT}<\\/string>/" \
  "${ROOT_DIR}/cmd/cleanroom-macos-guest-agent/${LABEL}.plist" > "${PLIST}"
chmod 0644 "${PLIST}"
xattr -cr "${SCRIPTS}" 2>/dev/null || true

cat > "${SCRIPTS}/postinstall" <<'EOF'
#!/bin/sh
set -eu

label="com.buildkite.cleanroom.macos-guest-agent"
script_dir=$(cd "$(dirname "$0")" && pwd)
target_volume=${3:-/}

case "${target_volume}" in
  ""|"/")
    target_root=""
    bootstrap=1
    ;;
  *)
    target_root=${target_volume%/}
    bootstrap=0
    ;;
esac

agent_path="${target_root}/usr/local/bin/cleanroom-macos-guest-agent"
plist="${target_root}/Library/LaunchDaemons/${label}.plist"

/usr/bin/install -d -o root -g wheel -m 0755 "${target_root}/usr/local/bin"
/usr/bin/install -d -o root -g wheel -m 0755 "${target_root}/Library/LaunchDaemons"
/usr/bin/install -o root -g wheel -m 0755 \
  "${script_dir}/cleanroom-macos-guest-agent" \
  "${agent_path}"
/usr/bin/install -o root -g wheel -m 0644 \
  "${script_dir}/${label}.plist" \
  "${plist}"

/usr/bin/xattr -c "${agent_path}" "${plist}" >/dev/null 2>&1 || true

if [ "${bootstrap}" -eq 1 ] && [ -x /bin/launchctl ] && [ -f "${plist}" ]; then
  /bin/launchctl bootout "system/${label}" >/dev/null 2>&1 || true
  /bin/launchctl bootstrap system "${plist}" >/dev/null 2>&1 || true
  /bin/launchctl kickstart -k "system/${label}" >/dev/null 2>&1 || true
fi

exit 0
EOF
chmod 0755 "${SCRIPTS}/postinstall"
xattr -cr "${SCRIPTS}" 2>/dev/null || true

COPYFILE_DISABLE=1 pkgbuild \
  --nopayload \
  --scripts "${SCRIPTS}" \
  --identifier "${LABEL}" \
  --version "${AGENT_VERSION}" \
  --install-location / \
  "${OUT}" >/dev/null

echo "${OUT}"
