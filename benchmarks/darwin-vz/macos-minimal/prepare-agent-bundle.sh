#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
AGENT_BIN="${ROOT_DIR}/dist/cleanroom-macos-guest-agent"
AGENT_VERSION="0.1.0"
BASE=""
OUT=""
FORCE=0
ALLOW_UNVERIFIED_OWNERSHIP=0

usage() {
  cat <<'EOF'
Usage:
  prepare-agent-bundle --base <base-bundle-dir> --out <prepared-bundle-dir> [options]

Options:
  --agent-bin <path>       Agent binary to install. Default: dist/cleanroom-macos-guest-agent.
  --agent-version <value>  Version to write to bundle.json. Default: 0.1.0.
  --allow-unverified-ownership
                            Continue when root ownership cannot be set. The
                            resulting bundle is for inspection only until a
                            live smoke proves launchd starts the agent.
  --force                  Replace an existing output directory.
  -h, --help               Show this help.

The script clones a local macOS VM bundle, mounts the APFS Data volume from the
cloned disk image, installs the Cleanroom macOS guest agent as a LaunchDaemon,
updates bundle.json, and leaves the base bundle untouched.
EOF
}

die() {
  echo "prepare-agent-bundle: $*" >&2
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --base)
      [[ $# -ge 2 ]] || die "missing value for --base"
      BASE="$2"
      shift 2
      ;;
    --out)
      [[ $# -ge 2 ]] || die "missing value for --out"
      OUT="$2"
      shift 2
      ;;
    --agent-bin)
      [[ $# -ge 2 ]] || die "missing value for --agent-bin"
      AGENT_BIN="$2"
      shift 2
      ;;
    --agent-version)
      [[ $# -ge 2 ]] || die "missing value for --agent-version"
      AGENT_VERSION="$2"
      shift 2
      ;;
    --force)
      FORCE=1
      shift
      ;;
    --allow-unverified-ownership)
      ALLOW_UNVERIFIED_OWNERSHIP=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

[[ -n "${BASE}" ]] || die "missing --base"
[[ -n "${OUT}" ]] || die "missing --out"
[[ -d "${BASE}" ]] || die "base bundle does not exist: ${BASE}"
[[ -f "${BASE}/bundle.json" ]] || die "base bundle missing bundle.json: ${BASE}"

if [[ ! -x "${AGENT_BIN}" ]]; then
  "${ROOT_DIR}/benchmarks/darwin-vz/macos-minimal/build-guest-agent.sh" "${AGENT_BIN}" >/dev/null
fi

[[ -x "${AGENT_BIN}" ]] || die "agent binary is not executable: ${AGENT_BIN}"

if [[ -e "${OUT}" && "${FORCE}" -ne 1 ]]; then
  die "output exists; use --force to replace: ${OUT}"
fi

OUT_PARENT="$(dirname "${OUT}")"
mkdir -p "${OUT_PARENT}"
TMP_OUT="${OUT_PARENT}/.$(basename "${OUT}").tmp.$$"
ATTACHED_DISK=""
DATA_VOLUME=""

cleanup() {
  if [[ -n "${DATA_VOLUME}" ]]; then
    diskutil unmount "${DATA_VOLUME}" >/dev/null 2>&1 || true
  fi
  if [[ -n "${ATTACHED_DISK}" ]]; then
    hdiutil detach "${ATTACHED_DISK}" >/dev/null 2>&1 || true
  fi
  if [[ -d "${TMP_OUT}" ]]; then
    rm -rf "${TMP_OUT}"
  fi
}
trap cleanup EXIT

rm -rf "${TMP_OUT}"
mkdir -p "${TMP_OUT}"

for name in bundle.json disk.img auxiliary.storage hardware-model.bin machine-identifier.bin; do
  [[ -f "${BASE}/${name}" ]] || die "base bundle missing ${name}: ${BASE}"
  cp -c "${BASE}/${name}" "${TMP_OUT}/${name}"
done

AGENT_PORT="$(
  /usr/bin/python3 - "${TMP_OUT}/bundle.json" <<'PY'
import json
import sys

with open(sys.argv[1], "r", encoding="utf-8") as f:
    manifest = json.load(f)
print(manifest.get("agent", {}).get("port", 10700))
PY
)"

ATTACH_OUTPUT="$(hdiutil attach -nomount "${TMP_OUT}/disk.img")"
ATTACHED_DISK="$(printf '%s\n' "${ATTACH_OUTPUT}" | awk '/GUID_partition_scheme/ {print $1; exit}')"
[[ -n "${ATTACHED_DISK}" ]] || die "could not identify attached disk"

MAIN_CONTAINER="$(diskutil list "${ATTACHED_DISK}" | awk '
  /Apple_APFS Container disk/ && $0 !~ /ISC|Recovery/ {
    for (i = 1; i <= NF; i++) {
      if ($i ~ /^disk[0-9]+$/ && $(i - 1) == "Container") {
        print $i
        exit
      }
    }
  }
')"
[[ -n "${MAIN_CONTAINER}" ]] || die "could not identify main APFS container"

DATA_VOLUME="$(diskutil apfs list "${MAIN_CONTAINER}" | awk '
  /APFS Volume Disk \(Role\):/ && /\(Data\)/ {
    for (i = 1; i <= NF; i++) {
      if ($i == "(Role):" && (i + 1) <= NF) {
        print $(i + 1)
        exit
      }
    }
  }
')"
[[ -n "${DATA_VOLUME}" ]] || die "could not identify APFS Data volume"

diskutil mount "${DATA_VOLUME}" >/dev/null
MOUNT_POINT="$(diskutil info "${DATA_VOLUME}" | awk -F: '/Mount Point/ {sub(/^[[:space:]]+/, "", $2); print $2; exit}')"
[[ -n "${MOUNT_POINT}" && "${MOUNT_POINT}" != "Not mounted" ]] || die "could not identify Data volume mount point"

install -d \
  "${MOUNT_POINT}/usr/local/bin" \
  "${MOUNT_POINT}/Library/LaunchDaemons" \
  "${MOUNT_POINT}/private/var/db" \
  "${MOUNT_POINT}/private/var/log"
install -m 0755 "${AGENT_BIN}" "${MOUNT_POINT}/usr/local/bin/cleanroom-macos-guest-agent"

PLIST_PATH="${MOUNT_POINT}/Library/LaunchDaemons/com.buildkite.cleanroom.macos-guest-agent.plist"
sed "s/<string>10700<\\/string>/<string>${AGENT_PORT}<\\/string>/" \
  "${ROOT_DIR}/cmd/cleanroom-macos-guest-agent/com.buildkite.cleanroom.macos-guest-agent.plist" > "${PLIST_PATH}"
chmod 0644 "${PLIST_PATH}"
touch "${MOUNT_POINT}/private/var/db/.AppleSetupDone"
chmod 0644 "${MOUNT_POINT}/private/var/db/.AppleSetupDone"

xattr -c "${MOUNT_POINT}/usr/local/bin/cleanroom-macos-guest-agent" "${PLIST_PATH}" 2>/dev/null || true

if ! chown 0:0 "${MOUNT_POINT}/usr/local/bin/cleanroom-macos-guest-agent" "${PLIST_PATH}" 2>/dev/null; then
  if [[ "${ALLOW_UNVERIFIED_OWNERSHIP}" -ne 1 ]]; then
    die "could not set root ownership on installed files; rerun with privileges, or pass --allow-unverified-ownership for an inspection-only bundle"
  fi
  echo "prepare-agent-bundle: warning: could not set root ownership on installed files; bundle is inspection-only until live smoke proves launchd starts the agent" >&2
fi

/usr/bin/python3 - "${TMP_OUT}/bundle.json" "${AGENT_VERSION}" <<'PY'
import json
import sys

path = sys.argv[1]
version = sys.argv[2]

with open(path, "r", encoding="utf-8") as f:
    manifest = json.load(f)

agent = manifest.setdefault("agent", {})
agent["transport"] = "virtio_socket"
agent["version"] = version

with open(path, "w", encoding="utf-8") as f:
    json.dump(manifest, f, indent=2, sort_keys=True)
    f.write("\n")
PY

diskutil unmount "${DATA_VOLUME}" >/dev/null
DATA_VOLUME=""
hdiutil detach "${ATTACHED_DISK}" >/dev/null
ATTACHED_DISK=""

if [[ -e "${OUT}" ]]; then
  rm -rf "${OUT}"
fi
mv "${TMP_OUT}" "${OUT}"
TMP_OUT=""

echo "prepared bundle: ${OUT}"
