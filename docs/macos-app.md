# macOS Menubar App Plan

## Goal

Ship a minimal macOS menubar host for cleanroom that:

- provides one primary action: `Enable Cleanroom`
- defaults to user-mode `launchd` service (no `sudo`) instead of asking user/system mode upfront
- shows lightweight runtime status in the menu
- installs a global CLI symlink at `/usr/local/bin/cleanroom` during enable flow (admin prompt)
- keeps system daemon install as an advanced action
- becomes the required host for Network Extension-based ingress/egress filtering per cleanroom run

The app is intentionally thin. Control-plane behavior stays in the existing Go server.

Primary reason for having a macOS app:

- Network Extension capabilities on macOS require an app bundle with the right signing + entitlements.
- A standalone CLI/helper binary cannot install and manage NE providers on its own.

## UX Impact

### Installation

- Today: users install CLI binaries and manually run `cleanroom serve` or `sudo cleanroom serve install`.
- With menubar app: users install/open `Cleanroom.app` and click `Enable Cleanroom`.
- Enable flow starts user service and installs `/usr/local/bin/cleanroom` symlink with one admin prompt.
- Privileged system daemon remains optional under `Advanced`.

### Usage

- Status is always available via a 👩‍🔬 menubar icon.
- One-click setup (`Enable Cleanroom`) handles service + CLI wiring.
- Ongoing user service control remains in `Advanced` actions.
- `Advanced` includes a `Run Server At Login` checkbox.
- `Advanced` includes `Enable Network Filter` / `Disable Network Filter` controls.
- Logs are discoverable from the menu.
- Existing CLI UX remains valid; app is additive.

## Architecture

### Components

- `cleanroom` (Go): still the only control-plane implementation.
- `Cleanroom.app` (Swift/AppKit): process supervisor + UX shell.
- `CleanroomFilterDataProvider` (Network Extension target): packet/flow decision point.
- `CleanroomFilterControlProvider` (optional NE target): policy control channel and rule updates.

### MVP runtime model

- Menubar app manages a per-user `launchd` LaunchAgent at `~/Library/LaunchAgents/com.buildkite.cleanroom.user.plist`.
- `Enable Cleanroom` bootstraps/kickstarts the LaunchAgent and ensures `/usr/local/bin/cleanroom` points to bundled CLI.
- Advanced actions can restart/stop user service and install the system daemon when needed.
- App now loads/saves `NEFilterManager` preferences for network-filter enable/disable requests and surfaces status/error in the menu.
- App bundle now includes `CleanroomFilterDataProvider.appex` from `macos/CleanroomFilterDataProvider/`.
- User LaunchAgent now exports `CLEANROOM_NETWORK_FILTER_POLICY_PATH` and `CLEANROOM_NETWORK_FILTER_TARGET_PROCESS` so `cleanroom serve` can publish filter policy snapshots.
- Service stdout/stderr is captured to `~/Library/Logs/cleanroom-user-server.log`.
- App logs remain in `~/Library/Logs/cleanroom-menubar.log`.
- Provider now enforces host/port allow rules for cleanroom helper traffic using policy snapshots; per-run scoping is still the next milestone.

### Privileged path

- Menu action runs `cleanroom serve install --force` using macOS admin authorization prompt (`osascript`).
- This reuses current daemon install logic and service file rendering in Go.
- Network filter enablement (next milestone) will also require explicit user/admin approval via System Settings prompts.

### Network Extension model (phase 2)

- The app enables a system network filter using `NEFilterManager`.
- `cleanroom serve` publishes a merged policy snapshot JSON at `CLEANROOM_NETWORK_FILTER_POLICY_PATH`.
- App passes policy metadata to the provider using `NEFilterProviderConfiguration.vendorConfiguration`.
- Provider enforces host/port allowlist decisions for flows whose source process matches `CLEANROOM_NETWORK_FILTER_TARGET_PROCESS`.
- If filter policy is unavailable, cleanroom should fail closed when filter mode is required.

Implementation notes:

- Use app-group shared state for policy snapshots and health/sequence markers.
- Keep policy format backend-neutral; extension consumes compiled allow/deny inputs rather than repo YAML directly.
- Add explicit run identity mapping so rules are scoped to each cleanroom run/sandbox.

## Bundling and Install

### Build output

- Build produces `dist/Cleanroom.app`.
- Bundle embeds `cleanroom` at `Contents/Helpers/cleanroom` so app startup does not depend on PATH.
- Bundle embeds `cleanroom-darwin-vz` at `Contents/Helpers/cleanroom-darwin-vz` so the darwin-vz backend can resolve its sibling helper path.
- Bundle embeds `cleanroom-guest-agent-linux-<arch>` at `Contents/Resources/` for darwin-vz guest bootstrapping on host architecture.
- Bundle embeds `CleanroomFilterDataProvider.appex` at `Contents/PlugIns/` and signs it with content-filter entitlements.
- Local ad-hoc builds do not apply network-extension entitlements to the host app so `Cleanroom.app` remains launchable.
- Host app network-extension entitlements are applied when using a real signing identity (`CLEANROOM_CODESIGN_IDENTITY`).

### Local install

- `mise run install:macos-app` copies the app to `~/Applications/Cleanroom.app`.
- User launches it directly from Finder/Spotlight.

### Network filter install flow

1. User installs/launches `Cleanroom.app`.
2. User chooses "Enable Network Filter" in the menubar app.
3. App requests approval to save/load NE filter preferences.
4. User approves the extension in macOS System Settings when prompted.
5. App verifies filter status and only then marks filtering as active for cleanroom runs.

## Distribution Direction

### Recommendation

- Start with self-distribution (release tarball/Homebrew cask style).
- Re-evaluate App Store later only if constraints change.

### Why

- App currently supervises a helper binary and offers privileged daemon installation.
- Those behaviors are usually a poor fit for App Store review and sandbox constraints.
- Network Extension distribution also requires specific entitlements and signing profile handling.

## Repository Strategy

Short term:

- Keep app in this repo while interfaces and UX settle.
- This preserves fast iteration against `cleanroom serve` lifecycle behavior.

When to split to its own repo:

- app release cadence diverges from CLI/backend
- dedicated CI/release notarization pipeline is needed
- app surface expands (settings, diagnostics, update framework)

## Phased Rollout

1. MVP (this change)
- Menubar icon + menu
- One-click `Enable Cleanroom` flow
- User LaunchAgent default + `/usr/local/bin/cleanroom` symlink install
- System daemon install in `Advanced`
- Open logs and quit

2. Network filtering (primary follow-up)
- add policy-sync plumbing from `cleanroom serve` into the bundled NE provider
- optional control provider target if bidirectional control path is needed
- enforce per-cleanroom run ingress/egress allowlists

3. Stability pass
- login item support
- better external state detection (existing daemon/process)
- diagnostics view and richer error reporting

4. Advanced networking (optional)
- if needed, introduce a separate Network Extension-capable host path
- keep backend-neutral CLI/API contracts in Go
