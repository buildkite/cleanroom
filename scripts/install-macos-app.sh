#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  exit 0
fi

APP_SRC="dist/Cleanroom.app"
APP_INSTALL_DIR="${CLEANROOM_APP_INSTALL_DIR:-$HOME/Applications}"
APP_DST="$APP_INSTALL_DIR/Cleanroom.app"
APP_USE_SUDO="${CLEANROOM_APP_USE_SUDO:-0}"
APP_BUNDLE_ID="com.buildkite.cleanroom.network"
APP_PROCESS_PATTERN="/Cleanroom.app/Contents/MacOS/Cleanroom"
APP_SRC="${CLEANROOM_APP_SRC:-$APP_SRC}"

script_dir() {
  CDPATH='' cd -- "$(dirname "$0")" >/dev/null && pwd
}

repo_root() {
  CDPATH='' cd -- "$(script_dir)/.." >/dev/null && pwd
}

run_install_command() {
  if [[ "$APP_USE_SUDO" == "1" ]]; then
    /usr/bin/sudo "$@"
    return
  fi
  "$@"
}

build_app_as_original_user_if_needed() {
  if [[ "$(id -u)" -ne 0 || -z "${SUDO_USER:-}" ]]; then
    return 0
  fi

  local repo_root_dir mise_bin user_home
  repo_root_dir="$(repo_root)"
  mise_bin="$(command -v mise || true)"
  if [[ -z "$mise_bin" ]]; then
    echo "mise is required to build the macOS app bundle" >&2
    exit 1
  fi

  user_home="$(dscl . -read "/Users/$SUDO_USER" NFSHomeDirectory 2>/dev/null | awk '{print $2}' || true)"
  if [[ -z "$user_home" ]]; then
    echo "failed to resolve home directory for $SUDO_USER" >&2
    exit 1
  fi

  echo "building Cleanroom.app as $SUDO_USER"
  (
    cd "$repo_root_dir"
    /usr/bin/sudo -H -u "$SUDO_USER" \
      env \
      HOME="$user_home" \
      PATH="$PATH" \
      CLEANROOM_CODESIGN_IDENTITY="${CLEANROOM_CODESIGN_IDENTITY:-}" \
      CLEANROOM_CODESIGN_APP_ENTITLEMENTS="${CLEANROOM_CODESIGN_APP_ENTITLEMENTS:-}" \
      CLEANROOM_MACOS_APP_PROFILE="${CLEANROOM_MACOS_APP_PROFILE:-}" \
      CLEANROOM_MACOS_FILTER_PROFILE="${CLEANROOM_MACOS_FILTER_PROFILE:-}" \
      "$mise_bin" run build:macos-app
  )
}

running_cleanroom_pids() {
  pgrep -f "$APP_PROCESS_PATTERN" || true
}

cleanroom_is_running() {
  pgrep -f "$APP_PROCESS_PATTERN" >/dev/null 2>&1
}

stop_running_cleanroom_app() {
  local pids pid

  pids="$(running_cleanroom_pids)"
  if [[ -z "$pids" ]]; then
    return 0
  fi

  echo "stopping running Cleanroom app"
  /usr/bin/osascript -e "tell application id \"$APP_BUNDLE_ID\" to quit" >/dev/null 2>&1 || true

  for _ in 1 2 3 4 5; do
    if ! cleanroom_is_running; then
      return 0
    fi
    sleep 1
  done

  while IFS= read -r pid; do
    [[ -n "$pid" ]] || continue
    kill -TERM "$pid" >/dev/null 2>&1 || true
  done <<< "$pids"

  for _ in 1 2 3; do
    if ! cleanroom_is_running; then
      return 0
    fi
    sleep 1
  done

  pids="$(running_cleanroom_pids)"
  while IFS= read -r pid; do
    [[ -n "$pid" ]] || continue
    kill -KILL "$pid" >/dev/null 2>&1 || true
  done <<< "$pids"

  sleep 1
  if cleanroom_is_running; then
    echo "failed to stop running Cleanroom app" >&2
    exit 1
  fi
}

if [[ ! -d "$APP_SRC" ]]; then
  build_app_as_original_user_if_needed
fi

if [[ ! -d "$APP_SRC" ]]; then
  echo "$APP_SRC is missing; run build:macos-app first" >&2
  exit 1
fi

stop_running_cleanroom_app

run_install_command mkdir -p "$APP_INSTALL_DIR"
run_install_command rm -rf "$APP_DST"
run_install_command /usr/bin/ditto "$APP_SRC" "$APP_DST"

echo "installed $APP_DST"
