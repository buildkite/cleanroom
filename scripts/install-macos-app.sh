#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  exit 0
fi

APP_SRC="dist/Cleanroom.app"
APP_INSTALL_DIR="${CLEANROOM_APP_INSTALL_DIR:-$HOME/Applications}"
APP_DST="$APP_INSTALL_DIR/Cleanroom.app"

if [[ ! -d "$APP_SRC" ]]; then
  echo "$APP_SRC is missing; run build:macos-app first" >&2
  exit 1
fi

mkdir -p "$APP_INSTALL_DIR"
rm -rf "$APP_DST"
cp -R "$APP_SRC" "$APP_DST"
codesign --force --sign - "$APP_DST" >/dev/null

echo "installed $APP_DST"
