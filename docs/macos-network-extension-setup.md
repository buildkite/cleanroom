# macOS Network Extension Setup

This document covers the Apple Developer setup required to build and run the
real-signed `Cleanroom.app` with the macOS Network Extension enabled.

Use this when:

- `Cleanroom.app` is signed with an `Apple Development` identity
- the app bundle includes the `CleanroomFilterDataProvider` system extension
- you want to enable the network filter from the CLI or inspect it manually from the macOS utility app

## Why this is needed

Ad-hoc signing is enough to build the app locally, but it is not enough to
manage Network Extension preferences reliably.

For a real-signed build, Apple requires:

- an App ID for the host app
- an App ID for the bundled Network Extension system extension
- `Network Extensions` enabled on both App IDs
- `System Extensions` enabled on the host app App ID
- a registered Mac development device
- a `Mac App Development` provisioning profile for each bundle ID

For local development there is one important entitlement split:

- `Apple Development` signing uses the standard `content-filter-provider` Network Extension entitlement,
  even though the provider is packaged as a `.systemextension`
- `Developer ID Application` signing for self-distribution uses the
  `content-filter-provider-systemextension` variant instead

Cleanroom now carries both forms:

- [macos/CleanroomFilterDataProvider/entitlements.plist](/Users/lachlan/.codex/worktrees/6db9/cleanroom/macos/CleanroomFilterDataProvider/entitlements.plist)
  for local Xcode and development-profile builds
- [macos/CleanroomFilterDataProvider/entitlements-developer-id.plist](/Users/lachlan/.codex/worktrees/6db9/cleanroom/macos/CleanroomFilterDataProvider/entitlements-developer-id.plist)
  for later Developer ID packaging

Without those profiles, macOS will reject the app at launch with an
`amfid` error such as `no eligible provisioning profiles found`.

## Find the bundle IDs

Do not guess the bundle IDs. Read them from the repo:

```bash
/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' macos/Info.plist
/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' macos/CleanroomFilterDataProvider/Info.plist
```

At the time of writing, those are the host app bundle ID and the filter
extension bundle ID. If either one changes, the App IDs and provisioning
profiles must match the new values exactly.

## Prerequisites

- an Apple Developer account on the correct team
- `Account Holder` or `Admin` access for App IDs, devices, and profiles
- an `Apple Development` certificate installed in your keychain
- Xcode installed

Check the signing identity currently available on this machine:

```bash
security find-identity -v -p codesigning
```

## Step 1: Register the App IDs

Open:

- [Certificates, Identifiers & Profiles > Identifiers](https://developer.apple.com/account/resources/identifiers/list)

Create two explicit App IDs:

- the host app bundle ID from `macos/Info.plist`
- the filter extension bundle ID from `macos/CleanroomFilterDataProvider/Info.plist`

Enable these capabilities:

- Host app App ID:
  - `Network Extensions`
  - `System Extensions`
- Network Extension system extension App ID:
  - `Network Extensions`

Notes:

- You need one App ID per bundle identifier.
- The host app and the `.systemextension` bundle are signed separately, so they need separate
  App IDs and separate provisioning profiles.
- If `Network Extensions` is not available in the App ID editor, check
  `Capability Requests` in the Apple Developer portal.

## Step 2: Register this Mac as a development device

Get the Mac's provisioning UDID:

```bash
system_profiler SPHardwareDataType | rg "Model Name|Provisioning UDID"
```

Then open:

- [Certificates, Identifiers & Profiles > Devices](https://developer.apple.com/account/resources/devices/list)

Create a new device:

- Platform: `macOS`
- Name: any clear name for this machine
- Device ID: the `Provisioning UDID`

Do not use the hardware UUID. For macOS provisioning, use the `Provisioning UDID`.

## Step 3: Create the provisioning profiles

Open:

- [Certificates, Identifiers & Profiles > Profiles](https://developer.apple.com/account/resources/profiles/list)

Create two profiles:

1. `Mac App Development` profile for the host app App ID
2. `Mac App Development` profile for the filter extension App ID

For each profile:

- select the same `Apple Development` certificate
- select this Mac as the device
- download the `.mobileprovision`

## Step 4: Build and install the app

The macOS app build script can auto-discover matching profiles from the
standard Xcode profile directories, but explicit paths are more predictable.

Set the profile paths:

```bash
export CLEANROOM_MACOS_APP_PROFILE="/path/to/host.mobileprovision"
export CLEANROOM_MACOS_FILTER_PROFILE="/path/to/filter.mobileprovision"
```

If you want to force a specific signing identity, also set:

```bash
export CLEANROOM_CODESIGN_IDENTITY="Apple Development: Your Name (TEAMID)"
```

Then build and install:

```bash
mise run install:macos-app-system
```

That flow:

- builds the app as your user
- embeds both provisioning profiles
- signs the app and the system extension
- uses `sudo` only for copying the app into `/Applications`

## Step 5: Enable the filter

Install the network-filter daemon first:

```bash
cleanroom network install
```

Then enable the filter from the CLI:

```bash
cleanroom network enable
```

macOS may prompt for approval in System Settings the first time the filter is enabled.
It may also prompt to approve the Cleanroom system extension before the filter can start.

Useful companion commands:

```bash
cleanroom network status
cleanroom network disable
cleanroom network reset
```

You can still open `/Applications/Cleanroom.app` manually for diagnostics if needed.

## Troubleshooting

### App ID does not appear in the profile wizard

The App ID has not been registered yet, or you are on the wrong team, or your
account role cannot create/select it.

### This Mac does not appear in the profile wizard

The device is not registered yet. Register it using the `Provisioning UDID`,
then recreate the profile if needed.

### The app says it cannot be opened

Check the signature and embedded profiles:

```bash
codesign -dvv /Applications/Cleanroom.app
codesign -dvv /Applications/Cleanroom.app/Contents/Library/SystemExtensions/com.buildkite.cleanroom.network.filter.systemextension
ls -l /Applications/Cleanroom.app/Contents/embedded.provisionprofile
ls -l /Applications/Cleanroom.app/Contents/Library/SystemExtensions/com.buildkite.cleanroom.network.filter.systemextension/Contents/embedded.provisionprofile
```

If the profiles are missing or do not match the bundle IDs, rebuild with the
correct profile paths.

If Xcode says the provisioning profile does not match the value of
`com.apple.developer.networking.networkextension`, check that the target is
using the development entitlements file and not the Developer ID one. For local
Apple Development signing, the profile will normally contain `content-filter-provider`,
not `content-filter-provider-systemextension`.

### `cleanroom network enable` fails with `permission denied`

This usually means one of:

- the app is ad-hoc signed
- the app was signed with the wrong identity
- the embedded profiles do not match the app and extension bundle IDs
- macOS still has stale filter preferences from an older build

Rebuild with the correct profiles, reinstall to `/Applications`, and if needed
remove the old filter entry in System Settings before re-enabling it.

### `cleanroom network enable` says the provider or system extension has not started

This usually means one of:

- the host app profile does not include `com.apple.developer.system-extension.install`
- the app or system extension profile does not permit the `content-filter-provider-systemextension` entitlement
- the system extension has not been approved in System Settings

Regenerate the host and system extension provisioning profiles after enabling the
`System Extensions` capability on the host App ID, reinstall the app, and retry.

## References

- [Register an App ID](https://developer.apple.com/help/account/identifiers/register-an-app-id/)
- [Enable app capabilities](https://developer.apple.com/help/account/identifiers/enable-app-capabilities/)
- [Register a single device](https://developer.apple.com/help/account/devices/register-a-single-device)
- [Devices overview](https://developer.apple.com/help/account/devices/devices-overview)
- [Create a development provisioning profile](https://developer.apple.com/help/account/provisioning-profiles/create-a-development-provisioning-profile/)
- [Provisioning with managed capabilities](https://developer.apple.com/help/account/reference/provisioning-with-managed-capabilities/)
