#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  exit 0
fi

APP_INSTALL_DIR="${CLEANROOM_APP_INSTALL_DIR:-$HOME/Applications}"
APP_DST="$APP_INSTALL_DIR/Cleanroom.app"

if [[ -d "$APP_DST" ]]; then
  rm -rf "$APP_DST"
  echo "removed $APP_DST"
else
  echo "nothing to remove at $APP_DST"
fi
