#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  exit 0
fi

APP_SRC="dist/Cleanroom.app"
APP_INSTALL_DIR="${CLEANROOM_APP_INSTALL_DIR:-$HOME/Applications}"
APP_DST="$APP_INSTALL_DIR/Cleanroom.app"
APP_ENTITLEMENTS="macos/entitlements.plist"
# Default to ad-hoc signing for local development installs.
CODE_SIGN_IDENTITY="${CLEANROOM_CODESIGN_IDENTITY:--}"
# By default, only apply host-app entitlements for non-ad-hoc signing.
APPLY_APP_ENTITLEMENTS="${CLEANROOM_CODESIGN_APP_ENTITLEMENTS:-}"
FILTER_PROVIDER_NAME="CleanroomFilterDataProvider"
FILTER_PROVIDER_ENTITLEMENTS="macos/CleanroomFilterDataProvider/entitlements.plist"
FILTER_PROVIDER_APPEX_DIR="$APP_DST/Contents/PlugIns/$FILTER_PROVIDER_NAME.appex"
FILTER_PROVIDER_APPEX_EXEC="$FILTER_PROVIDER_APPEX_DIR/Contents/MacOS/$FILTER_PROVIDER_NAME"

if [[ -z "$APPLY_APP_ENTITLEMENTS" ]]; then
  if [[ "$CODE_SIGN_IDENTITY" == "-" ]]; then
    APPLY_APP_ENTITLEMENTS="0"
  else
    APPLY_APP_ENTITLEMENTS="1"
  fi
fi

if [[ ! -d "$APP_SRC" ]]; then
  echo "$APP_SRC is missing; run build:macos-app first" >&2
  exit 1
fi

mkdir -p "$APP_INSTALL_DIR"
rm -rf "$APP_DST"
cp -R "$APP_SRC" "$APP_DST"
if [[ -d "$FILTER_PROVIDER_APPEX_DIR" ]]; then
  if [[ -f "$FILTER_PROVIDER_ENTITLEMENTS" ]]; then
    codesign --force --sign "$CODE_SIGN_IDENTITY" --entitlements "$FILTER_PROVIDER_ENTITLEMENTS" "$FILTER_PROVIDER_APPEX_EXEC" >/dev/null
    codesign --force --sign "$CODE_SIGN_IDENTITY" --entitlements "$FILTER_PROVIDER_ENTITLEMENTS" "$FILTER_PROVIDER_APPEX_DIR" >/dev/null
  else
    codesign --force --sign "$CODE_SIGN_IDENTITY" "$FILTER_PROVIDER_APPEX_EXEC" >/dev/null
    codesign --force --sign "$CODE_SIGN_IDENTITY" "$FILTER_PROVIDER_APPEX_DIR" >/dev/null
  fi
fi
if [[ "$APPLY_APP_ENTITLEMENTS" == "1" && -f "$APP_ENTITLEMENTS" ]]; then
  codesign --force --sign "$CODE_SIGN_IDENTITY" --entitlements "$APP_ENTITLEMENTS" "$APP_DST" >/dev/null
else
  codesign --force --sign "$CODE_SIGN_IDENTITY" "$APP_DST" >/dev/null
fi

echo "installed $APP_DST"
