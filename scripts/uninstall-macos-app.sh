#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  exit 0
fi

APP_INSTALL_DIR="${CLEANROOM_APP_INSTALL_DIR:-$HOME/Applications}"
APP_DST="$APP_INSTALL_DIR/Cleanroom.app"
APP_USE_SUDO="${CLEANROOM_APP_USE_SUDO:-0}"
APP_BUNDLE_ID="com.buildkite.cleanroom.network"
APP_PROCESS_PATTERN="/Cleanroom.app/Contents/MacOS/Cleanroom"

run_uninstall_command() {
  if [[ "$APP_USE_SUDO" == "1" ]]; then
    /usr/bin/sudo "$@"
    return
  fi
  "$@"
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

if [[ -d "$APP_DST" ]]; then
  stop_running_cleanroom_app
  run_uninstall_command rm -rf "$APP_DST"
  echo "removed $APP_DST"
else
  echo "nothing to remove at $APP_DST"
fi
