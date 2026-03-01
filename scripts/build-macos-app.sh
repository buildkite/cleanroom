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
HOST_ARCH="$(go env GOARCH)"
APP_GUEST_AGENT_BIN="$APP_RESOURCES_DIR/cleanroom-guest-agent-linux-$HOST_ARCH"

APP_ICON_SRC="macos/icon-1024.png"
APP_ICON_NAME="Cleanroom.icns"
MENUBAR_ICON_SRC="macos/menubar-icon.png"
MENUBAR_ICON_2X_SRC="macos/menubar-icon@2x.png"

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
mkdir -p "$APP_CONTENTS/MacOS" "$APP_HELPER_DIR" "$APP_RESOURCES_DIR"

xcrun swiftc -O -framework AppKit macos/main.swift -o "$APP_EXEC"
install -m 0644 macos/Info.plist "$APP_PLIST"
install -m 0755 dist/cleanroom "$APP_HELPER_BIN"
install -m 0755 dist/cleanroom-darwin-vz "$APP_DARWIN_VZ_HELPER_BIN"
install -m 0644 "dist/cleanroom-guest-agent-linux-$HOST_ARCH" "$APP_GUEST_AGENT_BIN"
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
codesign --force --sign - "$APP_DIR" >/dev/null

echo "built $APP_DIR"
