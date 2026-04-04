#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  exit 0
fi

APP_DIR="${CLEANROOM_MACOS_APP_OUTPUT_PATH:-dist/Cleanroom.app}"
APP_CONTENTS="$APP_DIR/Contents"
APP_EXEC="$APP_CONTENTS/MacOS/Cleanroom"
APP_HELPER_DIR="$APP_CONTENTS/Helpers"
APP_HELPER_BIN="$APP_HELPER_DIR/cleanroom"
APP_DARWIN_VZ_HELPER_APP="$APP_HELPER_DIR/cleanroom-darwin-vz.app"
APP_PLIST="$APP_CONTENTS/Info.plist"
APP_RESOURCES_DIR="$APP_CONTENTS/Resources"
APP_LIBRARY_DIR="$APP_CONTENTS/Library"
APP_SYSTEM_EXTENSIONS_DIR="$APP_LIBRARY_DIR/SystemExtensions"
HOST_ARCH="$(go env GOARCH)"
CLEANROOM_BINARY_SRC="${CLEANROOM_MACOS_CLEANROOM_BINARY:-dist/cleanroom}"
DARWIN_VZ_HELPER_APP_SRC="${CLEANROOM_MACOS_DARWIN_VZ_HELPER_APP:-dist/cleanroom-darwin-vz.app}"
GUEST_AGENT_BINARY_SRC="${CLEANROOM_MACOS_GUEST_AGENT_BINARY:-dist/cleanroom-guest-agent-linux-$HOST_ARCH}"
APP_GUEST_AGENT_BIN="$APP_RESOURCES_DIR/$(basename "$GUEST_AGENT_BINARY_SRC")"
SWIFT_TARGET="${CLEANROOM_MACOS_SWIFT_TARGET:-}"

APP_ICON_SRC="macos/icon-1024.png"
APP_ICON_NAME="Cleanroom.icns"
APP_ENTITLEMENTS="macos/entitlements.plist"
BUNDLE_SHORT_VERSION="${CLEANROOM_MACOS_SHORT_VERSION:-}"
BUNDLE_VERSION="${CLEANROOM_MACOS_BUNDLE_VERSION:-}"
# Default to the first available Apple Development identity when present.
CODE_SIGN_IDENTITY="${CLEANROOM_CODESIGN_IDENTITY:-}"
# By default, only apply host-app entitlements for non-ad-hoc signing.
APPLY_APP_ENTITLEMENTS="${CLEANROOM_CODESIGN_APP_ENTITLEMENTS:-}"
APP_PROVISIONING_PROFILE="${CLEANROOM_MACOS_APP_PROFILE:-}"
FILTER_PROVIDER_PROVISIONING_PROFILE="${CLEANROOM_MACOS_FILTER_PROFILE:-}"
FILTER_PROVIDER_NAME="CleanroomFilterDataProvider"
APP_SOURCES=(
  "macos/main.swift"
  "macos/NetworkFilterDaemon.swift"
)
FILTER_PROVIDER_SOURCES=(
  "macos/NetworkFilterDaemon.swift"
  "macos/CleanroomFilterDataProvider/main.swift"
  "macos/CleanroomFilterDataProvider/provider.swift"
)
FILTER_PROVIDER_PLIST_SRC="macos/CleanroomFilterDataProvider/Info.plist"
FILTER_PROVIDER_DEVELOPMENT_ENTITLEMENTS="macos/CleanroomFilterDataProvider/entitlements.plist"
FILTER_PROVIDER_DEVELOPER_ID_ENTITLEMENTS="macos/CleanroomFilterDataProvider/entitlements-developer-id.plist"
FILTER_PROVIDER_ENTITLEMENTS="$FILTER_PROVIDER_DEVELOPMENT_ENTITLEMENTS"
APP_BUNDLE_ID="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' macos/Info.plist)"
FILTER_PROVIDER_BUNDLE_ID="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' macos/CleanroomFilterDataProvider/Info.plist)"
FILTER_PROVIDER_SYSTEM_EXTENSION_DIR="$APP_SYSTEM_EXTENSIONS_DIR/$FILTER_PROVIDER_BUNDLE_ID.systemextension"
FILTER_PROVIDER_SYSTEM_EXTENSION_CONTENTS="$FILTER_PROVIDER_SYSTEM_EXTENSION_DIR/Contents"
FILTER_PROVIDER_SYSTEM_EXTENSION_EXEC="$FILTER_PROVIDER_SYSTEM_EXTENSION_CONTENTS/MacOS/$FILTER_PROVIDER_NAME"
FILTER_PROVIDER_SYSTEM_EXTENSION_PLIST="$FILTER_PROVIDER_SYSTEM_EXTENSION_CONTENTS/Info.plist"
APP_EMBEDDED_PROFILE="$APP_CONTENTS/embedded.provisionprofile"
FILTER_PROVIDER_EMBEDDED_PROFILE="$FILTER_PROVIDER_SYSTEM_EXTENSION_CONTENTS/embedded.provisionprofile"
APP_NETWORK_ENTITLEMENT="content-filter-provider"
APP_PROFILE_NETWORK_ENTITLEMENT="$APP_NETWORK_ENTITLEMENT"
FILTER_PROVIDER_NETWORK_ENTITLEMENT="content-filter-provider"

resolve_bundle_short_version() {
  if [[ -n "$BUNDLE_SHORT_VERSION" ]]; then
    printf '%s\n' "$BUNDLE_SHORT_VERSION"
    return 0
  fi

  local latest_tag
  latest_tag="$(git describe --tags --abbrev=0 2>/dev/null || true)"
  latest_tag="${latest_tag#v}"
  if [[ "$latest_tag" =~ ^[0-9]+(\.[0-9]+){0,2}$ ]]; then
    printf '%s\n' "$latest_tag"
    return 0
  fi

  printf '%s\n' "0.1.0"
}

resolve_bundle_version() {
  if [[ -n "$BUNDLE_VERSION" ]]; then
    printf '%s\n' "$BUNDLE_VERSION"
    return 0
  fi

  date -u +%Y%m%d%H%M%S
}

set_plist_string() {
  local plist_path="$1"
  local key="$2"
  local value="$3"

  /usr/bin/plutil -replace "$key" -string "$value" "$plist_path"
}

resolve_codesign_identity() {
  if [[ -n "$CODE_SIGN_IDENTITY" ]]; then
    printf '%s\n' "$CODE_SIGN_IDENTITY"
    return 0
  fi

  local identity
  identity="$(
    security find-identity -v -p codesigning 2>/dev/null |
      sed -n 's/.*"\(Apple Development:[^"]*\)".*/\1/p' |
      head -n 1
  )"
  if [[ -n "$identity" ]]; then
    printf '%s\n' "$identity"
    return 0
  fi

  printf '%s\n' "-"
}

CODE_SIGN_IDENTITY="$(resolve_codesign_identity)"
BUNDLE_SHORT_VERSION="$(resolve_bundle_short_version)"
BUNDLE_VERSION="$(resolve_bundle_version)"

if [[ "$CODE_SIGN_IDENTITY" == Developer\ ID\ Application:* ]]; then
  FILTER_PROVIDER_ENTITLEMENTS="$FILTER_PROVIDER_DEVELOPER_ID_ENTITLEMENTS"
  APP_PROFILE_NETWORK_ENTITLEMENT="content-filter-provider-systemextension"
  FILTER_PROVIDER_NETWORK_ENTITLEMENT="content-filter-provider-systemextension"
fi

profile_search_dirs=(
  "$HOME/Library/Developer/Xcode/UserData/Provisioning Profiles"
  "$HOME/Library/MobileDevice/Provisioning Profiles"
  "$HOME/Downloads"
)

decode_provisioning_profile() {
  local profile_path="$1"
  local plist_path="$2"

  security cms -D -i "$profile_path" >"$plist_path" 2>/dev/null
}

profile_platform_is_macos() {
  local plist_path="$1"
  local platform_dump

  platform_dump="$(/usr/libexec/PlistBuddy -c 'Print :Platform' "$plist_path" 2>/dev/null || true)"
  [[ "$platform_dump" == *"macOS"* || "$platform_dump" == *"OSX"* ]]
}

profile_application_identifier() {
  local plist_path="$1"
  /usr/libexec/PlistBuddy -c 'Print :Entitlements:application-identifier' "$plist_path" 2>/dev/null ||
    /usr/libexec/PlistBuddy -c 'Print :Entitlements:com.apple.application-identifier' "$plist_path" 2>/dev/null ||
    true
}

profile_supports_network_extension() {
  local plist_path="$1"
  local required_value="$2"
  local entitlement_dump

  entitlement_dump="$(
    /usr/libexec/PlistBuddy -c 'Print :Entitlements:com.apple.developer.networking.networkextension' "$plist_path" 2>/dev/null || true
  )"
  [[ "$entitlement_dump" == *"$required_value"* ]]
}

profile_supports_system_extension_install() {
  local plist_path="$1"
  local entitlement_value

  entitlement_value="$(
    /usr/libexec/PlistBuddy -c 'Print :Entitlements:com.apple.developer.system-extension.install' "$plist_path" 2>/dev/null || true
  )"
  [[ "$entitlement_value" == "true" || "$entitlement_value" == *"true"* ]]
}

bundle_id_matches_profile() {
  local bundle_id="$1"
  local app_identifier="$2"
  local pattern
  local prefix
  local suffix

  app_identifier="${app_identifier#*.}"
  if [[ -z "$app_identifier" ]]; then
    return 1
  fi

  pattern="$app_identifier"
  if [[ "$pattern" == *"*"* ]]; then
    prefix="${pattern%%\**}"
    suffix="${pattern#*\*}"
    [[ "$bundle_id" == "$prefix"*"$suffix" ]]
    return
  fi

  [[ "$bundle_id" == "$pattern" ]]
}

profile_matches_bundle_id() {
  local profile_path="$1"
  local bundle_id="$2"
  local required_network_extension="$3"
  local require_system_extension_install="$4"
  local profile_plist
  local app_identifier

  profile_plist="$(mktemp "${TMPDIR:-/tmp}/cleanroom-profile.XXXXXX.plist")"
  if ! decode_provisioning_profile "$profile_path" "$profile_plist"; then
    rm -f "$profile_plist"
    return 1
  fi
  if ! profile_platform_is_macos "$profile_plist"; then
    rm -f "$profile_plist"
    return 1
  fi
  app_identifier="$(profile_application_identifier "$profile_plist")"
  if ! bundle_id_matches_profile "$bundle_id" "$app_identifier"; then
    rm -f "$profile_plist"
    return 1
  fi
  if [[ -n "$required_network_extension" ]] && ! profile_supports_network_extension "$profile_plist" "$required_network_extension"; then
    rm -f "$profile_plist"
    return 1
  fi
  if [[ "$require_system_extension_install" == "1" ]] && ! profile_supports_system_extension_install "$profile_plist"; then
    rm -f "$profile_plist"
    return 1
  fi
  rm -f "$profile_plist"
  return 0
}

resolve_provisioning_profile() {
  local requested_path="$1"
  local bundle_id="$2"
  local env_var_name="$3"
  local required_network_extension="$4"
  local require_system_extension_install="$5"
  local candidate

  if [[ -n "$requested_path" ]]; then
    if [[ ! -f "$requested_path" ]]; then
      echo "$env_var_name=$requested_path does not exist" >&2
      exit 1
    fi
    if ! profile_matches_bundle_id "$requested_path" "$bundle_id" "$required_network_extension" "$require_system_extension_install"; then
      echo "$env_var_name=$requested_path is not an eligible macOS provisioning profile for $bundle_id" >&2
      exit 1
    fi
    printf '%s\n' "$requested_path"
    return 0
  fi

  for dir in "${profile_search_dirs[@]}"; do
    [[ -d "$dir" ]] || continue
    while IFS= read -r candidate; do
      [[ -n "$candidate" ]] || continue
      if profile_matches_bundle_id "$candidate" "$bundle_id" "$required_network_extension" "$require_system_extension_install"; then
        printf '%s\n' "$candidate"
        return 0
      fi
    done < <(find "$dir" -type f \( -name '*.mobileprovision' -o -name '*.provisionprofile' \) | sort)
  done

  echo "no eligible macOS provisioning profile found for $bundle_id; set $env_var_name=/path/to/profile.mobileprovision or build ad-hoc with CLEANROOM_CODESIGN_IDENTITY=-" >&2
  exit 1
}

if [[ -z "$APPLY_APP_ENTITLEMENTS" ]]; then
  if [[ "$CODE_SIGN_IDENTITY" == "-" ]]; then
    APPLY_APP_ENTITLEMENTS="0"
  else
    APPLY_APP_ENTITLEMENTS="1"
  fi
fi

if [[ "$CODE_SIGN_IDENTITY" != "-" ]]; then
  FILTER_PROVIDER_PROVISIONING_PROFILE="$(
    resolve_provisioning_profile \
      "$FILTER_PROVIDER_PROVISIONING_PROFILE" \
      "$FILTER_PROVIDER_BUNDLE_ID" \
      "CLEANROOM_MACOS_FILTER_PROFILE" \
      "$FILTER_PROVIDER_NETWORK_ENTITLEMENT" \
      "0"
  )"
  if [[ "$APPLY_APP_ENTITLEMENTS" == "1" ]]; then
    APP_PROVISIONING_PROFILE="$(
      resolve_provisioning_profile \
        "$APP_PROVISIONING_PROFILE" \
        "$APP_BUNDLE_ID" \
        "CLEANROOM_MACOS_APP_PROFILE" \
        "$APP_PROFILE_NETWORK_ENTITLEMENT" \
        "1"
    )"
  fi
fi

if [[ ! -x "$CLEANROOM_BINARY_SRC" ]]; then
  echo "$CLEANROOM_BINARY_SRC is missing; run build:go first" >&2
  exit 1
fi

if [[ ! -x "$DARWIN_VZ_HELPER_APP_SRC/Contents/MacOS/cleanroom-darwin-vz" ]]; then
  echo "$DARWIN_VZ_HELPER_APP_SRC is missing; run build:darwin first" >&2
  exit 1
fi

if [[ ! -f "$GUEST_AGENT_BINARY_SRC" ]]; then
  echo "$GUEST_AGENT_BINARY_SRC is missing; run build:go first" >&2
  exit 1
fi

rm -rf "$APP_DIR"
mkdir -p "$APP_CONTENTS/MacOS" "$APP_HELPER_DIR" "$APP_RESOURCES_DIR" "$FILTER_PROVIDER_SYSTEM_EXTENSION_CONTENTS/MacOS"

app_swiftc_args=(-O -framework AppKit -framework NetworkExtension -framework SystemExtensions)
if [[ -n "$SWIFT_TARGET" ]]; then
  app_swiftc_args+=(-target "$SWIFT_TARGET")
fi
app_swiftc_args+=("${APP_SOURCES[@]}" -o "$APP_EXEC")
xcrun swiftc "${app_swiftc_args[@]}"
install -m 0644 macos/Info.plist "$APP_PLIST"
install -m 0755 "$CLEANROOM_BINARY_SRC" "$APP_HELPER_BIN"
/usr/bin/ditto "$DARWIN_VZ_HELPER_APP_SRC" "$APP_DARWIN_VZ_HELPER_APP"
install -m 0644 "$GUEST_AGENT_BINARY_SRC" "$APP_GUEST_AGENT_BIN"
provider_swiftc_args=(-O -module-name "$FILTER_PROVIDER_NAME" -framework NetworkExtension)
if [[ -n "$SWIFT_TARGET" ]]; then
  provider_swiftc_args+=(-target "$SWIFT_TARGET")
fi
provider_swiftc_args+=("${FILTER_PROVIDER_SOURCES[@]}" -o "$FILTER_PROVIDER_SYSTEM_EXTENSION_EXEC" -lbsm)
xcrun swiftc "${provider_swiftc_args[@]}"
install -m 0644 "$FILTER_PROVIDER_PLIST_SRC" "$FILTER_PROVIDER_SYSTEM_EXTENSION_PLIST"
set_plist_string "$APP_PLIST" "CFBundleShortVersionString" "$BUNDLE_SHORT_VERSION"
set_plist_string "$APP_PLIST" "CFBundleVersion" "$BUNDLE_VERSION"
set_plist_string "$FILTER_PROVIDER_SYSTEM_EXTENSION_PLIST" "CFBundleShortVersionString" "$BUNDLE_SHORT_VERSION"
set_plist_string "$FILTER_PROVIDER_SYSTEM_EXTENSION_PLIST" "CFBundleVersion" "$BUNDLE_VERSION"
provider_class="$(
  /usr/libexec/PlistBuddy \
    -c 'Print :NetworkExtension:NEProviderClasses:com.apple.networkextension.filter-packet' \
    "$FILTER_PROVIDER_SYSTEM_EXTENSION_PLIST" 2>/dev/null || true
)"
unresolved_build_setting="\$("
if [[ -z "$provider_class" ]]; then
  echo "system extension Info.plist is missing NetworkExtension provider class metadata" >&2
  exit 1
fi
if [[ "$provider_class" == *"$unresolved_build_setting"* ]]; then
  echo "system extension Info.plist contains an unresolved build setting in the provider class: $provider_class" >&2
  exit 1
fi
if [[ -n "$FILTER_PROVIDER_PROVISIONING_PROFILE" ]]; then
  install -m 0644 "$FILTER_PROVIDER_PROVISIONING_PROFILE" "$FILTER_PROVIDER_EMBEDDED_PROFILE"
fi
if [[ -n "$APP_PROVISIONING_PROFILE" ]]; then
  install -m 0644 "$APP_PROVISIONING_PROFILE" "$APP_EMBEDDED_PROFILE"
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
codesign --force --sign "$CODE_SIGN_IDENTITY" "$APP_HELPER_BIN" >/dev/null
if [[ -f "$FILTER_PROVIDER_ENTITLEMENTS" ]]; then
  codesign --force --sign "$CODE_SIGN_IDENTITY" --entitlements "$FILTER_PROVIDER_ENTITLEMENTS" "$FILTER_PROVIDER_SYSTEM_EXTENSION_EXEC" >/dev/null
  codesign --force --sign "$CODE_SIGN_IDENTITY" --entitlements "$FILTER_PROVIDER_ENTITLEMENTS" "$FILTER_PROVIDER_SYSTEM_EXTENSION_DIR" >/dev/null
else
  codesign --force --sign "$CODE_SIGN_IDENTITY" "$FILTER_PROVIDER_SYSTEM_EXTENSION_EXEC" >/dev/null
  codesign --force --sign "$CODE_SIGN_IDENTITY" "$FILTER_PROVIDER_SYSTEM_EXTENSION_DIR" >/dev/null
fi
if [[ "$APPLY_APP_ENTITLEMENTS" == "1" && -f "$APP_ENTITLEMENTS" ]]; then
  codesign --force --sign "$CODE_SIGN_IDENTITY" --entitlements "$APP_ENTITLEMENTS" "$APP_DIR" >/dev/null
else
  codesign --force --sign "$CODE_SIGN_IDENTITY" "$APP_DIR" >/dev/null
fi

echo "built $APP_DIR ($BUNDLE_SHORT_VERSION/$BUNDLE_VERSION)"
