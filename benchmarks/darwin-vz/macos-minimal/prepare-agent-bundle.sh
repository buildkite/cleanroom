#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
AGENT_BIN="${ROOT_DIR}/dist/cleanroom-macos-guest-agent"
AGENT_VERSION="0.1.0"
BASE=""
OUT=""
FORCE=0
ALLOW_UNVERIFIED_OWNERSHIP=0
INSTALL_MODE="launchdaemon"
AGENT_USER="cleanroom"
AGENT_PASSWORD="cleanroom"
AGENT_UID="$(id -u)"
AGENT_GID="$(id -g)"

usage() {
  cat <<'EOF'
Usage:
  prepare-agent-bundle --base <base-bundle-dir> --out <prepared-bundle-dir> [options]

Options:
  --agent-bin <path>       Agent binary to install. Default: dist/cleanroom-macos-guest-agent.
  --agent-version <value>  Version to write to bundle.json. Default: 0.1.0.
  --install-mode <mode>    Agent startup mode: launchdaemon or user-cron.
                            Default: launchdaemon.
  --agent-user <name>      User to create for user-cron mode. Default: cleanroom.
  --agent-password <value> Password for the user-cron user. Default: cleanroom.
  --agent-uid <uid>        UID for the user-cron user. Default: current host UID.
  --agent-gid <gid>        GID for the user-cron user. Default: current host GID.
  --allow-unverified-ownership
                            Continue when root ownership cannot be set. The
                            resulting bundle is for inspection only until a
                            live smoke proves launchd starts the agent.
  --force                  Replace an existing output directory.
  -h, --help               Show this help.

The script clones a local macOS VM bundle, mounts the APFS Data volume from the
cloned disk image, installs the Cleanroom macOS guest agent, updates
bundle.json, and leaves the base bundle untouched.
EOF
}

die() {
  echo "prepare-agent-bundle: $*" >&2
  exit 1
}

copy_bundle_file() {
  local src="$1"
  local dst="$2"
  cp -c "${src}" "${dst}" 2>/dev/null || cp "${src}" "${dst}"
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
    --install-mode)
      [[ $# -ge 2 ]] || die "missing value for --install-mode"
      INSTALL_MODE="$2"
      shift 2
      ;;
    --agent-user)
      [[ $# -ge 2 ]] || die "missing value for --agent-user"
      AGENT_USER="$2"
      shift 2
      ;;
    --agent-password)
      [[ $# -ge 2 ]] || die "missing value for --agent-password"
      AGENT_PASSWORD="$2"
      shift 2
      ;;
    --agent-uid)
      [[ $# -ge 2 ]] || die "missing value for --agent-uid"
      AGENT_UID="$2"
      shift 2
      ;;
    --agent-gid)
      [[ $# -ge 2 ]] || die "missing value for --agent-gid"
      AGENT_GID="$2"
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
case "${INSTALL_MODE}" in
  launchdaemon|user-cron)
    ;;
  *)
    die "unsupported --install-mode ${INSTALL_MODE}; expected launchdaemon or user-cron"
    ;;
esac
[[ "${AGENT_UID}" =~ ^[0-9]+$ ]] || die "--agent-uid must be numeric"
[[ "${AGENT_GID}" =~ ^[0-9]+$ ]] || die "--agent-gid must be numeric"
[[ -n "${AGENT_USER}" ]] || die "--agent-user must not be empty"
if [[ "${INSTALL_MODE}" == "user-cron" && "${AGENT_UID}" -lt 501 ]]; then
  die "--agent-uid must be 501 or greater for user-cron mode"
fi

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
  copy_bundle_file "${BASE}/${name}" "${TMP_OUT}/${name}"
done

read -r AGENT_PORT MACOS_VERSION MACOS_BUILD < <(
  /usr/bin/python3 - "${TMP_OUT}/bundle.json" <<'PY'
import json
import sys

with open(sys.argv[1], "r", encoding="utf-8") as f:
    manifest = json.load(f)
print(
    manifest.get("agent", {}).get("port", 10700),
    manifest.get("macos_version", ""),
    manifest.get("macos_build", ""),
)
PY
)

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

if [[ "${INSTALL_MODE}" == "launchdaemon" ]]; then
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
else
  if [[ "${AGENT_UID}" != "$(id -u)" || "${AGENT_GID}" != "$(id -g)" ]]; then
    echo "prepare-agent-bundle: warning: user-cron mode works rootlessly only when --agent-uid/--agent-gid match the current host user; ownership must be verified by live smoke" >&2
  fi
  chmod u+rwx \
    "${MOUNT_POINT}/private/var/db/dslocal/nodes/Default" \
    "${MOUNT_POINT}/private/var/db/dslocal/nodes/Default/users" \
    "${MOUNT_POINT}/private/var/db/dslocal/nodes/Default/groups" 2>/dev/null || true

  /usr/bin/python3 - "${MOUNT_POINT}" "${AGENT_USER}" "${AGENT_PASSWORD}" "${AGENT_UID}" "${AGENT_GID}" "${AGENT_PORT}" "${AGENT_VERSION}" "${MACOS_VERSION}" "${MACOS_BUILD}" <<'PY'
import hashlib
import os
import plistlib
import sys
import uuid
from pathlib import Path

mount = Path(sys.argv[1])
user = sys.argv[2]
password = sys.argv[3].encode()
uid = sys.argv[4]
gid = sys.argv[5]
port = sys.argv[6]
version = sys.argv[7]
macos_version = sys.argv[8]
macos_build = sys.argv[9]
guid = str(uuid.uuid4()).upper()

target = mount / "private/var/db/dslocal/nodes/Default"
users_dir = target / "users"
groups_dir = target / "groups"
if not users_dir.is_dir():
    raise SystemExit(f"dslocal users directory not found: {users_dir}")
if not groups_dir.is_dir():
    raise SystemExit(f"dslocal groups directory not found: {groups_dir}")
if (users_dir / f"{user}.plist").exists():
    raise SystemExit(f"guest user already exists: {user}")
for existing_user_path in users_dir.glob("*.plist"):
    try:
        with existing_user_path.open("rb") as f:
            existing_user = plistlib.load(f)
    except Exception:
        continue
    if uid in existing_user.get("uid", []):
        existing_name = existing_user_path.stem
        raise SystemExit(f"guest uid {uid} already belongs to {existing_name}")

salt = os.urandom(32)
entropy = hashlib.pbkdf2_hmac("sha512", password, salt, 35000, dklen=128)
shadow = plistlib.dumps({
    "SALTED-SHA512-PBKDF2": {
        "entropy": entropy,
        "salt": salt,
        "iterations": 35000,
    }
}, fmt=plistlib.FMT_BINARY)

user_record = {
    "ShadowHashData": [shadow],
    "authentication_authority": [";ShadowHash;HASHLIST:<SALTED-SHA512-PBKDF2>"],
    "generateduid": [guid],
    "gid": [gid],
    "home": [f"/Users/{user}"],
    "name": [user],
    "passwd": ["********"],
    "realname": ["Cleanroom CI"],
    "shell": ["/bin/zsh"],
    "uid": [uid],
}
user_path = users_dir / f"{user}.plist"
with user_path.open("wb") as f:
    plistlib.dump(user_record, f, fmt=plistlib.FMT_BINARY)
os.chmod(user_path, 0o600)

admin_path = groups_dir / "admin.plist"
with admin_path.open("rb") as f:
    admin = plistlib.load(f)
admin.setdefault("users", [])
admin.setdefault("groupmembers", [])
if user not in admin["users"]:
    admin["users"].append(user)
if guid not in admin["groupmembers"]:
    admin["groupmembers"].append(guid)
with admin_path.open("wb") as f:
    plistlib.dump(admin, f, fmt=plistlib.FMT_BINARY)
os.chmod(admin_path, 0o600)

(mount / "private/var/db/.AppleSetupDone").touch()
os.chmod(mount / "private/var/db/.AppleSetupDone", 0o644)

def write_plist(path, values):
    path.parent.mkdir(parents=True, exist_ok=True)
    current = {}
    if path.exists():
        with path.open("rb") as f:
            current = plistlib.load(f)
    current.update(values)
    with path.open("wb") as f:
        plistlib.dump(current, f, fmt=plistlib.FMT_BINARY)
    os.chmod(path, 0o644)

setup_values = {
    "DidSeeAccessibility": True,
    "DidSeeActivationLock": True,
    "DidSeeAppearanceSetup": True,
    "DidSeeApplePaySetup": True,
    "DidSeeAppStore": True,
    "DidSeeCloudSetup": True,
    "DidSeeiCloudLoginForStorageServices": True,
    "DidSeeLockdownMode": True,
    "DidSeePrivacy": True,
    "DidSeeScreenTime": True,
    "DidSeeSiriSetup": True,
    "DidSeeSyncSetup": True,
    "DidSeeSyncSetup2": True,
    "DidSeeTermsOfAddress": True,
    "DidSeeTouchIDSetup": True,
    "DidSeeTrueTonePrivacy": True,
    "GestureMovieSeen": "none",
    "InitialAccountOnMac": True,
    "LastSeenBuddyBuildVersion": macos_build,
    "LastSeenCloudProductVersion": "99.99",
    "MiniBuddyLaunchedPostMigration": True,
    "MiniBuddyLaunchReason": 0,
    "MiniBuddyShouldLaunchToResumeSetup": False,
    "PreviousBuildVersion": macos_build,
    "PreviousSystemVersion": macos_version,
    "SkipExpressSettingsUpdating": True,
    "SkipFirstLoginOptimization": True,
}
for rel in [
    f"Users/{user}/Library/Preferences/com.apple.SetupAssistant.plist",
    "Library/Preferences/com.apple.SetupAssistant.plist",
]:
    write_plist(mount / rel, setup_values)

login_values = {
    "autoLoginUser": user,
    "GuestEnabled": False,
    "lastUser": "loggedIn",
    "lastUserName": user,
    "MiniBuddyLaunch": False,
    "MiniBuddyLaunchCount": 0,
    "oneTimeSSMigrationComplete": True,
    "RecentUsers": [user],
}
write_plist(mount / "Library/Preferences/com.apple.loginwindow.plist", login_values)
write_plist(mount / f"Users/{user}/Library/Preferences/com.apple.loginwindow.plist", {
    "MiniBuddyLaunch": False,
    "MiniBuddyLaunchCount": 0,
    "oneTimeSSMigrationComplete": True,
})

software_update_values = {
    "AutomaticCheckEnabled": False,
    "AutomaticDownload": False,
    "AutomaticallyInstallMacOSUpdates": False,
    "ConfigDataInstall": False,
    "CriticalUpdateInstall": False,
    "PostSuccessfulMinorUpdatePostLogOutNotification": False,
    "RecommendedUpdates": [],
}
if macos_build and macos_version:
    software_update_values["LastAttemptBuildVersion"] = f"{macos_version} ({macos_build})"
    software_update_values["LastAttemptSystemVersion"] = f"{macos_version} ({macos_build})"
write_plist(mount / "Library/Preferences/com.apple.SoftwareUpdate.plist", software_update_values)
write_plist(mount / f"Users/{user}/Library/Preferences/com.apple.SoftwareUpdate.plist", software_update_values)

key = bytes([0x7D, 0x89, 0x52, 0x23, 0xD2, 0xBC, 0xDD, 0xEA, 0xA3, 0xB9, 0x1F])
plain = bytearray(password + b"\x00")
while len(plain) % len(key) != 0:
    plain.append(0)
encoded = bytes(b ^ key[i % len(key)] for i, b in enumerate(plain))
kcpassword = mount / "private/etc/kcpassword"
kcpassword.parent.mkdir(parents=True, exist_ok=True)
kcpassword.write_bytes(encoded)
os.chmod(kcpassword, 0o600)

for rel in [
    f"Users/{user}/bin",
    f"Users/{user}/Library/Logs",
    f"Users/{user}/Library/LaunchAgents",
    f"Users/{user}/Desktop",
    f"Users/{user}/Documents",
    f"Users/{user}/Downloads",
    "private/var/at/tabs",
]:
    (mount / rel).mkdir(parents=True, exist_ok=True)

cron = mount / f"private/var/at/tabs/{user}"
cron.write_text(
    "SHELL=/bin/sh\n"
    f"CLEANROOM_VSOCK_PORT={port}\n"
    f"PATH=/usr/bin:/bin:/usr/sbin:/sbin:/Users/{user}/bin\n"
    f"* * * * * /usr/bin/pgrep -f '/Users/{user}/bin/cleanroom-macos-guest-agent' >/dev/null || "
    f"/Users/{user}/bin/cleanroom-macos-guest-agent "
    f">>/Users/{user}/Library/Logs/cleanroom-macos-guest-agent.cron.log "
    f"2>>/Users/{user}/Library/Logs/cleanroom-macos-guest-agent.cron.err\n",
    encoding="utf-8",
)
os.chmod(cron, 0o600)

launch_agent = mount / f"Users/{user}/Library/LaunchAgents/com.buildkite.cleanroom.macos-guest-agent.plist"
with launch_agent.open("wb") as f:
    plistlib.dump({
        "Label": "com.buildkite.cleanroom.macos-guest-agent",
        "ProgramArguments": [f"/Users/{user}/bin/cleanroom-macos-guest-agent"],
        "EnvironmentVariables": {"CLEANROOM_VSOCK_PORT": port},
        "RunAtLoad": True,
        "KeepAlive": True,
        "StandardOutPath": f"/Users/{user}/Library/Logs/cleanroom-macos-guest-agent.log",
        "StandardErrorPath": f"/Users/{user}/Library/Logs/cleanroom-macos-guest-agent.err.log",
    }, f, fmt=plistlib.FMT_BINARY)
os.chmod(launch_agent, 0o644)

print(f"configured user-cron agent user={user} uid={uid} gid={gid} port={port} version={version}")
PY

  install -m 0755 "${AGENT_BIN}" "${MOUNT_POINT}/Users/${AGENT_USER}/bin/cleanroom-macos-guest-agent"
  xattr -cr \
    "${MOUNT_POINT}/Users/${AGENT_USER}/bin/cleanroom-macos-guest-agent" \
    "${MOUNT_POINT}/Users/${AGENT_USER}/Library/LaunchAgents/com.buildkite.cleanroom.macos-guest-agent.plist" \
    "${MOUNT_POINT}/private/var/at/tabs/${AGENT_USER}" 2>/dev/null || true
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
