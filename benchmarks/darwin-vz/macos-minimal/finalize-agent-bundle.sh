#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
BASE=""
OUT=""
FORCE=0
AGENT_BIN="${ROOT_DIR}/dist/cleanroom-macos-guest-agent"
AGENT_VERSION="0.1.0"
AGENT_USER="cleanroom"
AGENT_PASSWORD="cleanroom"
AGENT_UID="$(id -u)"
AGENT_GID="$(id -g)"
PROFILE="headless"
USER_AGENT_PORT=""
RUNNER="${ROOT_DIR}/dist/darwin-vz-macos-minimal"
TIMEOUT=240
METRICS_DIR=""
KEEP_BOOTSTRAP=0
LABEL="com.buildkite.cleanroom.macos-guest-agent"
FINALIZED_MARKER="/private/var/db/cleanroom-macos-guest-agent.finalized"

usage() {
  cat <<'EOF'
Usage:
  finalize-agent-bundle --base <base-bundle-dir> --out <finalized-bundle-dir> [options]

Options:
  --agent-bin <path>       Agent binary to install. Default: dist/cleanroom-macos-guest-agent.
  --agent-version <value>  Version to write to bundle.json. Default: 0.1.0.
  --agent-user <name>      Temporary bootstrap user. Default: cleanroom.
  --agent-uid <uid>        UID for the temporary bootstrap user. Default: current host UID.
  --agent-gid <gid>        GID for the temporary bootstrap user. Default: current host GID.
  --profile <name>         Image profile to finalize: headless or gui. Default: headless.
  --user-agent-port <port> User LaunchAgent vsock port for --profile gui. Default: agent port + 1.
  --runner <path>          macOS runner binary. Default: dist/darwin-vz-macos-minimal.
  --timeout <seconds>      Timeout for each VM boot/exec. Default: 240.
  --metrics-dir <path>     Keep finalize.json, smoke.json, and optional gui-smoke.json metrics in this directory.
  --keep-bootstrap         Keep the temporary bootstrap bundle after a failure.
  --force                  Replace an existing output directory.
  -h, --help               Show this help.

The script clones a base macOS VM bundle, creates a temporary rootless
user-cron bootstrap bundle, boots it once to install the root-owned
LaunchDaemon from inside the guest, then boots it again to prove exec is served
by the LaunchDaemon as root. The gui profile keeps a real autologin user and
adds a user LaunchAgent on a separate vsock port.
EOF
}

die() {
  echo "finalize-agent-bundle: $*" >&2
  exit 1
}

redact_finalize_metrics() {
  /usr/bin/python3 - "$1" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, "r", encoding="utf-8") as f:
    metrics = json.load(f)
metrics["command"] = ["/bin/sh", "-lc", "<cleanroom macOS LaunchDaemon finalizer>"]
with open(path, "w", encoding="utf-8") as f:
    json.dump(metrics, f, indent=2, sort_keys=True)
    f.write("\n")
PY
}

remove_bootstrap_user_offline() (
  set -euo pipefail

  local bundle="$1"
  local user="$2"
  local profile="$3"
  local attached_disk=""
  local data_volume=""

  # shellcheck disable=SC2329 # invoked by the EXIT trap in this subshell
  cleanup_offline_mount() {
    if [[ -n "${data_volume}" ]]; then
      diskutil unmount "${data_volume}" >/dev/null 2>&1 || true
    fi
    if [[ -n "${attached_disk}" ]]; then
      hdiutil detach "${attached_disk}" >/dev/null 2>&1 || true
    fi
  }
  trap cleanup_offline_mount EXIT

  local attach_output
  attach_output="$(hdiutil attach -nomount "${bundle}/disk.img")"
  attached_disk="$(printf '%s\n' "${attach_output}" | awk '/GUID_partition_scheme/ {print $1; exit}')"
  [[ -n "${attached_disk}" ]] || die "could not identify attached disk for offline bootstrap cleanup"

  local main_container
  main_container="$(diskutil list "${attached_disk}" | awk '
    /Apple_APFS Container disk/ && $0 !~ /ISC|Recovery/ {
      for (i = 1; i <= NF; i++) {
        if ($i ~ /^disk[0-9]+$/ && $(i - 1) == "Container") {
          print $i
          exit
        }
      }
    }
  ')"
  [[ -n "${main_container}" ]] || die "could not identify APFS container for offline bootstrap cleanup"

  data_volume="$(diskutil apfs list "${main_container}" | awk '
    /APFS Volume Disk \(Role\):/ && /\(Data\)/ {
      for (i = 1; i <= NF; i++) {
        if ($i == "(Role):" && (i + 1) <= NF) {
          print $(i + 1)
          exit
        }
      }
    }
  ')"
  [[ -n "${data_volume}" ]] || die "could not identify Data volume for offline bootstrap cleanup"

  diskutil mount "${data_volume}" >/dev/null
  local mount_point
  mount_point="$(diskutil info "${data_volume}" | awk -F: '/Mount Point/ {sub(/^[[:space:]]+/, "", $2); print $2; exit}')"
  [[ -n "${mount_point}" && "${mount_point}" != "Not mounted" ]] || die "could not identify Data volume mount point for offline bootstrap cleanup"

  chmod u+rwx \
    "${mount_point}/private/var/db/dslocal/nodes/Default" \
    "${mount_point}/private/var/db/dslocal/nodes/Default/users" \
    "${mount_point}/private/var/db/dslocal/nodes/Default/groups" 2>/dev/null || true

  /usr/bin/python3 - "${mount_point}" "${user}" "${profile}" <<'PY'
import plistlib
import shutil
import sys
from pathlib import Path

mount = Path(sys.argv[1])
user = sys.argv[2]
profile = sys.argv[3]
remove_user = profile == "headless"
node = mount / "private/var/db/dslocal/nodes/Default"
user_path = node / "users" / f"{user}.plist"
guid = None

if remove_user and user_path.exists():
    with user_path.open("rb") as f:
        record = plistlib.load(f)
    values = record.get("generateduid") or []
    if values:
        guid = values[0]
    user_path.unlink()

groups_dir = node / "groups"
if remove_user and groups_dir.is_dir():
    for group_path in groups_dir.glob("*.plist"):
        with group_path.open("rb") as f:
            group = plistlib.load(f)
        changed = False
        users = [value for value in group.get("users", []) if value != user]
        if users != group.get("users", []):
            group["users"] = users
            changed = True
        if guid:
            members = [value for value in group.get("groupmembers", []) if value != guid]
            if members != group.get("groupmembers", []):
                group["groupmembers"] = members
                changed = True
        if changed:
            with group_path.open("wb") as f:
                plistlib.dump(group, f, fmt=plistlib.FMT_BINARY)

loginwindow = mount / "Library/Preferences/com.apple.loginwindow.plist"
if remove_user and loginwindow.exists():
    with loginwindow.open("rb") as f:
        values = plistlib.load(f)
    for key in ["autoLoginUser", "lastUserName"]:
        if values.get(key) == user:
            values.pop(key, None)
    if values.get("RecentUsers"):
        values["RecentUsers"] = [value for value in values["RecentUsers"] if value != user]
    with loginwindow.open("wb") as f:
        plistlib.dump(values, f, fmt=plistlib.FMT_BINARY)

paths = [f"private/var/at/tabs/{user}"]
if remove_user:
    paths.extend([
        f"Users/{user}",
        "private/etc/kcpassword",
    ])

for rel in paths:
    path = mount / rel
    if path.is_dir():
        shutil.rmtree(path)
    elif path.exists():
        path.unlink()
PY

  diskutil unmount "${data_volume}" >/dev/null
  data_volume=""
  hdiutil detach "${attached_disk}" >/dev/null
  attached_disk=""

  if [[ "${profile}" == "headless" ]]; then
    echo "removed offline bootstrap user: ${user}"
  else
    echo "removed offline bootstrap cron: ${user}"
  fi
)

write_bundle_profile_metadata() {
  /usr/bin/python3 - "${TMP_BOOTSTRAP}/bundle.json" "${PROFILE}" "${AGENT_VERSION}" "${USER_AGENT_PORT}" "${AGENT_USER}" <<'PY'
import json
import sys

path = sys.argv[1]
profile = sys.argv[2]
version = sys.argv[3]
user_agent_port = int(sys.argv[4])
user = sys.argv[5]

with open(path, "r", encoding="utf-8") as f:
    manifest = json.load(f)

manifest["image_profile"] = profile
if profile == "gui":
    manifest["user_agent"] = {
        "transport": "virtio_socket",
        "port": user_agent_port,
        "version": version,
        "user": user,
    }
else:
    manifest.pop("user_agent", None)

with open(path, "w", encoding="utf-8") as f:
    json.dump(manifest, f, indent=2, sort_keys=True)
    f.write("\n")
PY
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
    --agent-user)
      [[ $# -ge 2 ]] || die "missing value for --agent-user"
      AGENT_USER="$2"
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
    --profile)
      [[ $# -ge 2 ]] || die "missing value for --profile"
      PROFILE="$2"
      shift 2
      ;;
    --user-agent-port)
      [[ $# -ge 2 ]] || die "missing value for --user-agent-port"
      USER_AGENT_PORT="$2"
      shift 2
      ;;
    --runner)
      [[ $# -ge 2 ]] || die "missing value for --runner"
      RUNNER="$2"
      shift 2
      ;;
    --timeout)
      [[ $# -ge 2 ]] || die "missing value for --timeout"
      TIMEOUT="$2"
      shift 2
      ;;
    --metrics-dir)
      [[ $# -ge 2 ]] || die "missing value for --metrics-dir"
      METRICS_DIR="$2"
      shift 2
      ;;
    --keep-bootstrap)
      KEEP_BOOTSTRAP=1
      shift
      ;;
    --force)
      FORCE=1
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
[[ -n "${AGENT_VERSION}" ]] || die "--agent-version must not be empty"
[[ "${AGENT_UID}" =~ ^[0-9]+$ ]] || die "--agent-uid must be numeric"
[[ "${AGENT_GID}" =~ ^[0-9]+$ ]] || die "--agent-gid must be numeric"
[[ "${AGENT_UID}" -ge 501 ]] || die "--agent-uid must be 501 or greater"
[[ "${AGENT_USER}" =~ ^[A-Za-z_][A-Za-z0-9_-]*$ ]] || die "--agent-user must be a simple local account name"
case "${PROFILE}" in
  headless|gui)
    ;;
  *)
    die "--profile must be headless or gui"
    ;;
esac
case "${TIMEOUT}" in
  ''|*[!0-9]*)
    die "--timeout must be a positive integer"
    ;;
esac
[[ "${TIMEOUT}" -gt 0 ]] || die "--timeout must be a positive integer"

if [[ -e "${OUT}" && "${FORCE}" -ne 1 ]]; then
  die "output exists; use --force to replace: ${OUT}"
fi

if [[ ! -x "${AGENT_BIN}" ]]; then
  "${ROOT_DIR}/benchmarks/darwin-vz/macos-minimal/build-guest-agent.sh" "${AGENT_BIN}" >/dev/null
fi
[[ -x "${AGENT_BIN}" ]] || die "agent binary is not executable: ${AGENT_BIN}"

if [[ ! -x "${RUNNER}" ]]; then
  "${ROOT_DIR}/benchmarks/darwin-vz/macos-minimal/build-runner.sh" "${RUNNER}" >/dev/null
fi
[[ -x "${RUNNER}" ]] || die "runner binary is not executable: ${RUNNER}"

OUT_PARENT="$(dirname "${OUT}")"
mkdir -p "${OUT_PARENT}"
TMP_BOOTSTRAP="${OUT_PARENT}/.$(basename "${OUT}").bootstrap.$$"
TMP_METRICS=""
if [[ -n "${METRICS_DIR}" ]]; then
  mkdir -p "${METRICS_DIR}"
  FINALIZE_METRICS="${METRICS_DIR}/finalize.json"
  SMOKE_METRICS="${METRICS_DIR}/smoke.json"
  GUI_SMOKE_METRICS="${METRICS_DIR}/gui-smoke.json"
else
  TMP_METRICS="$(mktemp -d "${TMPDIR:-/tmp}/cleanroom-macos-finalize-metrics.XXXXXX")"
  FINALIZE_METRICS="${TMP_METRICS}/finalize.json"
  SMOKE_METRICS="${TMP_METRICS}/smoke.json"
  GUI_SMOKE_METRICS="${TMP_METRICS}/gui-smoke.json"
fi

cleanup() {
  if [[ -n "${TMP_METRICS}" && -d "${TMP_METRICS}" ]]; then
    rm -rf "${TMP_METRICS}"
  fi
  if [[ "${KEEP_BOOTSTRAP}" -ne 1 && -d "${TMP_BOOTSTRAP}" ]]; then
    rm -rf "${TMP_BOOTSTRAP}"
  fi
}
trap cleanup EXIT

rm -rf "${TMP_BOOTSTRAP}"

PREPARE_ARGS=(
  --base "${BASE}" \
  --out "${TMP_BOOTSTRAP}" \
  --agent-bin "${AGENT_BIN}" \
  --agent-version "${AGENT_VERSION}" \
  --install-mode user-cron \
  --agent-user "${AGENT_USER}" \
  --agent-password "${AGENT_PASSWORD}" \
  --agent-uid "${AGENT_UID}" \
  --agent-gid "${AGENT_GID}" \
  --force
)
if [[ "${PROFILE}" == "headless" ]]; then
  PREPARE_ARGS+=(--user-cron-no-autologin)
fi

"${ROOT_DIR}/benchmarks/darwin-vz/macos-minimal/prepare-agent-bundle.sh" "${PREPARE_ARGS[@]}"

AGENT_PORT="$(
  /usr/bin/python3 - "${TMP_BOOTSTRAP}/bundle.json" <<'PY'
import json
import sys

with open(sys.argv[1], "r", encoding="utf-8") as f:
    manifest = json.load(f)
print(manifest.get("agent", {}).get("port", 10700))
PY
)"
[[ "${AGENT_PORT}" =~ ^[0-9]+$ && "${AGENT_PORT}" -gt 0 ]] || die "bundle agent port is invalid: ${AGENT_PORT}"
if [[ -z "${USER_AGENT_PORT}" ]]; then
  USER_AGENT_PORT="$((AGENT_PORT + 1))"
fi
case "${USER_AGENT_PORT}" in
  ''|*[!0-9]*)
    die "--user-agent-port must be a positive integer"
    ;;
esac
[[ "${USER_AGENT_PORT}" -gt 0 ]] || die "--user-agent-port must be a positive integer"
[[ "${USER_AGENT_PORT}" != "${AGENT_PORT}" ]] || die "--user-agent-port must differ from the root agent port"

PASSWORD_HEX="$(printf '%s' "${AGENT_PASSWORD}" | /usr/bin/xxd -p -c 256)"

FINALIZER_SCRIPT="$(cat <<'EOF_FINALIZER'
set -eu
label="__LABEL__"
bootstrap_user="__AGENT_USER__"
password_hex="__PASSWORD_HEX__"
password="$(printf '%s' "${password_hex}" | /usr/bin/xxd -r -p)"
plist="/tmp/${label}.plist"
root_script="/tmp/${label}.finalize-root.$$"

cat > "${plist}" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>__LABEL__</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/cleanroom-macos-guest-agent</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>CLEANROOM_VSOCK_PORT</key>
    <string>__AGENT_PORT__</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>/var/log/cleanroom-macos-guest-agent.log</string>
  <key>StandardErrorPath</key>
  <string>/var/log/cleanroom-macos-guest-agent.err.log</string>
</dict>
</plist>
PLIST

cat > "${root_script}" <<'ROOT_SCRIPT'
#!/bin/sh
set -eu

label="__LABEL__"
bootstrap_user="__AGENT_USER__"
profile="__PROFILE__"
user_agent_port="__USER_AGENT_PORT__"
marker="__FINALIZED_MARKER__"
agent_src="/Users/${bootstrap_user}/bin/cleanroom-macos-guest-agent"
agent_dst="/usr/local/bin/cleanroom-macos-guest-agent"
plist_src="/tmp/${label}.plist"
plist_dst="/Library/LaunchDaemons/${label}.plist"
bootstrap_agent="/Users/${bootstrap_user}/Library/LaunchAgents/${label}.plist"
bootstrap_cron="/private/var/at/tabs/${bootstrap_user}"

test -x "${agent_src}"
test -f "${plist_src}"
/usr/bin/install -d -o root -g wheel -m 0755 /usr/local/bin /Library/LaunchDaemons /var/log
/bin/mkdir -p "$(/usr/bin/dirname "${marker}")"
/usr/bin/install -o root -g wheel -m 0755 "${agent_src}" "${agent_dst}"
/usr/bin/install -o root -g wheel -m 0644 "${plist_src}" "${plist_dst}"
/usr/bin/xattr -c "${agent_dst}" "${plist_dst}" >/dev/null 2>&1 || true

if [ -f "${bootstrap_cron}" ] && ! /usr/bin/grep -q "cleanroom-macos-guest-agent.finalized" "${bootstrap_cron}"; then
  echo "bootstrap cron is missing finalized marker check: ${bootstrap_cron}" >&2
  exit 1
fi

/usr/bin/touch "${marker}"
/usr/sbin/chown root:wheel "${marker}"
/bin/chmod 0644 "${marker}"

/bin/rm -f "${bootstrap_agent}" >/dev/null 2>&1 || true
if [ "${profile}" = "gui" ]; then
  /bin/mkdir -p "$(/usr/bin/dirname "${bootstrap_agent}")"
  cat > "${bootstrap_agent}" <<USER_AGENT_PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${label}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${agent_dst}</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>CLEANROOM_VSOCK_PORT</key>
    <string>${user_agent_port}</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>/Users/${bootstrap_user}/Library/Logs/cleanroom-macos-guest-agent.log</string>
  <key>StandardErrorPath</key>
  <string>/Users/${bootstrap_user}/Library/Logs/cleanroom-macos-guest-agent.err.log</string>
</dict>
</plist>
USER_AGENT_PLIST
  /usr/sbin/chown "${bootstrap_user}:staff" "${bootstrap_agent}" >/dev/null 2>&1 || true
  /bin/chmod 0644 "${bootstrap_agent}"
else
  /usr/bin/defaults delete /Library/Preferences/com.apple.loginwindow autoLoginUser >/dev/null 2>&1 || true
  /bin/rm -f /private/etc/kcpassword >/dev/null 2>&1 || true
fi
if /usr/bin/dscl . -read "/Users/${bootstrap_user}" >/dev/null 2>&1; then
  /usr/sbin/dseditgroup -o edit -d "${bootstrap_user}" -t user admin >/dev/null 2>&1 || true
fi

/usr/sbin/chown root:wheel "${agent_dst}" "${plist_dst}"
/bin/chmod 0755 "${agent_dst}"
/bin/chmod 0644 "${plist_dst}"
/usr/bin/stat -f "%Su:%Sg %Lp %N" "${agent_dst}" "${plist_dst}" "${marker}"
/bin/sync
/bin/sleep 5
ROOT_SCRIPT

/bin/chmod 0700 "${root_script}"
printf '%s\n' "${password}" | /usr/bin/sudo -S -p '' /bin/sh "${root_script}"
/bin/rm -f "${root_script}" "${plist}"
EOF_FINALIZER
)"
FINALIZER_SCRIPT="${FINALIZER_SCRIPT//__LABEL__/${LABEL}}"
FINALIZER_SCRIPT="${FINALIZER_SCRIPT//__AGENT_USER__/${AGENT_USER}}"
FINALIZER_SCRIPT="${FINALIZER_SCRIPT//__AGENT_PORT__/${AGENT_PORT}}"
FINALIZER_SCRIPT="${FINALIZER_SCRIPT//__USER_AGENT_PORT__/${USER_AGENT_PORT}}"
FINALIZER_SCRIPT="${FINALIZER_SCRIPT//__PROFILE__/${PROFILE}}"
FINALIZER_SCRIPT="${FINALIZER_SCRIPT//__PASSWORD_HEX__/${PASSWORD_HEX}}"
FINALIZER_SCRIPT="${FINALIZER_SCRIPT//__FINALIZED_MARKER__/${FINALIZED_MARKER}}"

SMOKE_SCRIPT="$(cat <<'EOF_SMOKE'
set -eu
label="__LABEL__"
bootstrap_user="__AGENT_USER__"
profile="__PROFILE__"
user_agent_port="__USER_AGENT_PORT__"
marker="__FINALIZED_MARKER__"
uid="$(id -u)"
user="$(id -un)"
printf "user=%s uid=%s cwd=%s\n" "${user}" "${uid}" "${PWD}"
test "${uid}" = "0"
/bin/launchctl print "system/${label}" >/dev/null
test -f "${marker}"
/usr/bin/stat -f "%Su:%Sg %Lp %N" /usr/local/bin/cleanroom-macos-guest-agent "/Library/LaunchDaemons/${label}.plist" "${marker}"
test ! -e "/private/var/at/tabs/${bootstrap_user}"
echo "bootstrap_cron=absent"
if [ "${profile}" = "gui" ]; then
  /usr/bin/id "${bootstrap_user}" >/dev/null
  if /usr/bin/dsmemberutil checkmembership -U "${bootstrap_user}" -G admin | /usr/bin/grep -q "is a member"; then
    echo "bootstrap_user=admin" >&2
    exit 1
  fi
  echo "bootstrap_admin=absent"
  test -f "/Users/${bootstrap_user}/Library/LaunchAgents/${label}.plist"
  /usr/bin/grep -q "<string>${user_agent_port}</string>" "/Users/${bootstrap_user}/Library/LaunchAgents/${label}.plist"
  echo "bootstrap_user=present"
else
  if /usr/bin/id "${bootstrap_user}" >/dev/null 2>&1; then
    echo "bootstrap_user=present" >&2
    exit 1
  fi
  test ! -e "/Users/${bootstrap_user}"
  echo "bootstrap_user=absent"
fi
EOF_SMOKE
)"
SMOKE_SCRIPT="${SMOKE_SCRIPT//__LABEL__/${LABEL}}"
SMOKE_SCRIPT="${SMOKE_SCRIPT//__AGENT_USER__/${AGENT_USER}}"
SMOKE_SCRIPT="${SMOKE_SCRIPT//__PROFILE__/${PROFILE}}"
SMOKE_SCRIPT="${SMOKE_SCRIPT//__USER_AGENT_PORT__/${USER_AGENT_PORT}}"
SMOKE_SCRIPT="${SMOKE_SCRIPT//__FINALIZED_MARKER__/${FINALIZED_MARKER}}"

GUI_SMOKE_SCRIPT="$(cat <<'EOF_GUI_SMOKE'
set -eu
label="__LABEL__"
expected_user="__AGENT_USER__"
uid="$(id -u)"
user="$(id -un)"
screenshot="/tmp/cleanroom-gui-smoke.png"
printf "gui_user=%s uid=%s cwd=%s\n" "${user}" "${uid}" "${PWD}"
test "${user}" = "${expected_user}"
test "${uid}" != "0"
/bin/launchctl print "gui/${uid}/${label}" >/dev/null
/usr/bin/killall TextEdit >/dev/null 2>&1 || true
/bin/rm -f "${screenshot}"
/usr/bin/open -a TextEdit
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
  if /usr/bin/pgrep -x TextEdit >/dev/null 2>&1; then
    break
  fi
  /bin/sleep 1
done
/usr/bin/pgrep -x TextEdit >/dev/null
if screenshot_output="$(/usr/sbin/screencapture -x "${screenshot}" 2>&1)"; then
  test -s "${screenshot}"
  /usr/bin/stat -f "gui_screenshot=%z %N" "${screenshot}"
else
  echo "gui_screenshot=unavailable"
  if [ -n "${screenshot_output}" ]; then
    printf "%s\n" "${screenshot_output}" >&2
  fi
fi
/usr/bin/killall TextEdit >/dev/null 2>&1 || true
EOF_GUI_SMOKE
)"
GUI_SMOKE_SCRIPT="${GUI_SMOKE_SCRIPT//__LABEL__/${LABEL}}"
GUI_SMOKE_SCRIPT="${GUI_SMOKE_SCRIPT//__AGENT_USER__/${AGENT_USER}}"

finalize_status=0
"${RUNNER}" \
  --bundle "${TMP_BOOTSTRAP}" \
  --timeout "${TIMEOUT}" \
  --metrics "${FINALIZE_METRICS}" \
  -- /bin/sh -lc "${FINALIZER_SCRIPT}" || finalize_status=$?
if [[ -f "${FINALIZE_METRICS}" ]]; then
  redact_finalize_metrics "${FINALIZE_METRICS}"
fi
if [[ "${finalize_status}" -ne 0 ]]; then
  exit "${finalize_status}"
fi
remove_bootstrap_user_offline "${TMP_BOOTSTRAP}" "${AGENT_USER}" "${PROFILE}"
write_bundle_profile_metadata

"${RUNNER}" \
  --bundle "${TMP_BOOTSTRAP}" \
  --timeout "${TIMEOUT}" \
  --metrics "${SMOKE_METRICS}" \
  -- /bin/sh -lc "${SMOKE_SCRIPT}"

if [[ "${PROFILE}" == "gui" ]]; then
  "${RUNNER}" \
    --bundle "${TMP_BOOTSTRAP}" \
    --agent user \
    --timeout "${TIMEOUT}" \
    --metrics "${GUI_SMOKE_METRICS}" \
    -- /bin/sh -lc "${GUI_SMOKE_SCRIPT}"
fi

if [[ -e "${OUT}" ]]; then
  if [[ "${FORCE}" -ne 1 ]]; then
    die "output appeared during finalization; use --force to replace: ${OUT}"
  fi
  rm -rf "${OUT}"
fi
mv "${TMP_BOOTSTRAP}" "${OUT}"
TMP_BOOTSTRAP=""

echo "finalized bundle: ${OUT}"
SUMMARY_METRICS=("${FINALIZE_METRICS}" "${SMOKE_METRICS}")
if [[ "${PROFILE}" == "gui" ]]; then
  SUMMARY_METRICS+=("${GUI_SMOKE_METRICS}")
fi
/usr/bin/python3 - "${SUMMARY_METRICS[@]}" <<'PY'
import json
import sys

for label, path in zip(["finalize", "smoke", "gui_smoke"], sys.argv[1:]):
    with open(path, "r", encoding="utf-8") as f:
        metrics = json.load(f)
    print(
        f"{label}: exit_code={metrics.get('exit_code')} "
        f"vsock_connect_ms={metrics.get('vsock_connect_ms')} "
        f"exec_response_ms={metrics.get('exec_response_ms')}"
    )
PY
