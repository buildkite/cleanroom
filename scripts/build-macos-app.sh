#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  exit 0
fi

APP_DIR="dist/Cleanroom.app"
APP_CONTENTS="$APP_DIR/Contents"
APP_EXEC="$APP_CONTENTS/MacOS/Cleanroom"
APP_HELPER_DIR="$APP_CONTENTS/Helpers"
APP_HELPER_BIN="$APP_HELPER_DIR/cleanroom"
APP_DARWIN_VZ_HELPER_BIN="$APP_HELPER_DIR/cleanroom-darwin-vz"
APP_PLIST="$APP_CONTENTS/Info.plist"
APP_RESOURCES_DIR="$APP_CONTENTS/Resources"
APP_PLUGINS_DIR="$APP_CONTENTS/PlugIns"
HOST_ARCH="$(go env GOARCH)"
APP_GUEST_AGENT_BIN="$APP_RESOURCES_DIR/cleanroom-guest-agent-linux-$HOST_ARCH"

APP_ICON_SRC="macos/icon-1024.png"
APP_ICON_NAME="Cleanroom.icns"
MENUBAR_ICON_SRC="macos/menubar-icon.png"
MENUBAR_ICON_2X_SRC="macos/menubar-icon@2x.png"
APP_ENTITLEMENTS="macos/entitlements.plist"
# Default to ad-hoc signing for local development builds.
CODE_SIGN_IDENTITY="${CLEANROOM_CODESIGN_IDENTITY:--}"
# By default, only apply host-app entitlements for non-ad-hoc signing.
APPLY_APP_ENTITLEMENTS="${CLEANROOM_CODESIGN_APP_ENTITLEMENTS:-}"
FILTER_PROVIDER_NAME="CleanroomFilterDataProvider"
FILTER_PROVIDER_SRC="macos/CleanroomFilterDataProvider/provider.swift"
FILTER_PROVIDER_PLIST_SRC="macos/CleanroomFilterDataProvider/Info.plist"
FILTER_PROVIDER_ENTITLEMENTS="macos/CleanroomFilterDataProvider/entitlements.plist"
FILTER_PROVIDER_APPEX_DIR="$APP_PLUGINS_DIR/$FILTER_PROVIDER_NAME.appex"
FILTER_PROVIDER_APPEX_CONTENTS="$FILTER_PROVIDER_APPEX_DIR/Contents"
FILTER_PROVIDER_APPEX_EXEC="$FILTER_PROVIDER_APPEX_CONTENTS/MacOS/$FILTER_PROVIDER_NAME"
FILTER_PROVIDER_APPEX_PLIST="$FILTER_PROVIDER_APPEX_CONTENTS/Info.plist"

if [[ -z "$APPLY_APP_ENTITLEMENTS" ]]; then
  if [[ "$CODE_SIGN_IDENTITY" == "-" ]]; then
    APPLY_APP_ENTITLEMENTS="0"
  else
    APPLY_APP_ENTITLEMENTS="1"
  fi
fi

if [[ ! -x "dist/cleanroom" ]]; then
  echo "dist/cleanroom is missing; run build:go first" >&2
  exit 1
fi

if [[ ! -x "dist/cleanroom-darwin-vz" ]]; then
  echo "dist/cleanroom-darwin-vz is missing; run build:darwin first" >&2
  exit 1
fi

if [[ ! -f "dist/cleanroom-guest-agent-linux-$HOST_ARCH" ]]; then
  echo "dist/cleanroom-guest-agent-linux-$HOST_ARCH is missing; run build:go first" >&2
  exit 1
fi

rm -rf "$APP_DIR"
mkdir -p "$APP_CONTENTS/MacOS" "$APP_HELPER_DIR" "$APP_RESOURCES_DIR" "$FILTER_PROVIDER_APPEX_CONTENTS/MacOS"

xcrun swiftc -O -framework AppKit -framework NetworkExtension macos/main.swift -o "$APP_EXEC"
install -m 0644 macos/Info.plist "$APP_PLIST"
install -m 0755 dist/cleanroom "$APP_HELPER_BIN"
install -m 0755 dist/cleanroom-darwin-vz "$APP_DARWIN_VZ_HELPER_BIN"
install -m 0644 "dist/cleanroom-guest-agent-linux-$HOST_ARCH" "$APP_GUEST_AGENT_BIN"
xcrun swiftc -O -module-name "$FILTER_PROVIDER_NAME" -framework NetworkExtension "$FILTER_PROVIDER_SRC" -o "$FILTER_PROVIDER_APPEX_EXEC"
install -m 0644 "$FILTER_PROVIDER_PLIST_SRC" "$FILTER_PROVIDER_APPEX_PLIST"
if [[ -f "$MENUBAR_ICON_SRC" ]]; then
  install -m 0644 "$MENUBAR_ICON_SRC" "$APP_RESOURCES_DIR/menubar-icon.png"
fi
if [[ -f "$MENUBAR_ICON_2X_SRC" ]]; then
  install -m 0644 "$MENUBAR_ICON_2X_SRC" "$APP_RESOURCES_DIR/menubar-icon@2x.png"
fi
if [[ -f "$APP_ICON_SRC" ]]; then
  ICONSET_DIR="$(mktemp -d "${TMPDIR:-/tmp}/cleanroom-icon.XXXXXX.iconset")"
  trap 'rm -rf "$ICONSET_DIR"' EXIT
  sips -z 16 16 "$APP_ICON_SRC" --out "$ICONSET_DIR/icon_16x16.png" >/dev/null
  sips -z 32 32 "$APP_ICON_SRC" --out "$ICONSET_DIR/icon_16x16@2x.png" >/dev/null
  sips -z 32 32 "$APP_ICON_SRC" --out "$ICONSET_DIR/icon_32x32.png" >/dev/null
  sips -z 64 64 "$APP_ICON_SRC" --out "$ICONSET_DIR/icon_32x32@2x.png" >/dev/null
  sips -z 128 128 "$APP_ICON_SRC" --out "$ICONSET_DIR/icon_128x128.png" >/dev/null
  sips -z 256 256 "$APP_ICON_SRC" --out "$ICONSET_DIR/icon_128x128@2x.png" >/dev/null
  sips -z 256 256 "$APP_ICON_SRC" --out "$ICONSET_DIR/icon_256x256.png" >/dev/null
  sips -z 512 512 "$APP_ICON_SRC" --out "$ICONSET_DIR/icon_256x256@2x.png" >/dev/null
  sips -z 512 512 "$APP_ICON_SRC" --out "$ICONSET_DIR/icon_512x512.png" >/dev/null
  cp "$APP_ICON_SRC" "$ICONSET_DIR/icon_512x512@2x.png"
  iconutil -c icns "$ICONSET_DIR" -o "$APP_RESOURCES_DIR/$APP_ICON_NAME"
  rm -rf "$ICONSET_DIR"
  trap - EXIT
fi
if [[ -f "$FILTER_PROVIDER_ENTITLEMENTS" ]]; then
  codesign --force --sign "$CODE_SIGN_IDENTITY" --entitlements "$FILTER_PROVIDER_ENTITLEMENTS" "$FILTER_PROVIDER_APPEX_EXEC" >/dev/null
  codesign --force --sign "$CODE_SIGN_IDENTITY" --entitlements "$FILTER_PROVIDER_ENTITLEMENTS" "$FILTER_PROVIDER_APPEX_DIR" >/dev/null
else
  codesign --force --sign "$CODE_SIGN_IDENTITY" "$FILTER_PROVIDER_APPEX_EXEC" >/dev/null
  codesign --force --sign "$CODE_SIGN_IDENTITY" "$FILTER_PROVIDER_APPEX_DIR" >/dev/null
fi
if [[ "$APPLY_APP_ENTITLEMENTS" == "1" && -f "$APP_ENTITLEMENTS" ]]; then
  codesign --force --sign "$CODE_SIGN_IDENTITY" --entitlements "$APP_ENTITLEMENTS" "$APP_DIR" >/dev/null
else
  codesign --force --sign "$CODE_SIGN_IDENTITY" "$APP_DIR" >/dev/null
fi

echo "built $APP_DIR"
