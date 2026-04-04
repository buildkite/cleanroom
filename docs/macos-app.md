# macOS Network Utility Plan

## Goal

Ship a minimal macOS app whose only job is to host Cleanroom's Network Extension
integration for darwin-vz egress filtering.

The app exists because macOS requires an app bundle, signing, and entitlements to
manage Network Extension providers. It should not become a second control plane.

## Product Boundary

### The CLI owns

- `cleanroom serve`
- `cleanroom daemon install/start/stop/status/uninstall`
- helper resolution
- runtime config
- sandbox lifecycle
- version and doctor reporting

### The macOS app owns

- hosting the Apple APIs needed to enable and disable the network filter
- requesting macOS approval for the Network Extension
- acting as a support bridge for `cleanroom network enable|disable|reset|status`

### The macOS app does not own

- starting or stopping the Cleanroom server
- installing or repairing the CLI
- creating `/usr/local/bin/cleanroom`
- choosing user vs system daemon mode
- supervising launchd jobs
- a persistent menu bar or settings UI

This keeps the app thin and keeps all normal Cleanroom usage in the CLI.

## UX

### Installation

1. User installs the CLI using the normal Cleanroom install path.
2. On macOS, the default signed package install also installs `Cleanroom.app` into `/Applications`.
3. User installs the network-filter daemon from the CLI:
   `cleanroom network install`
4. User enables the network filter from the CLI:
   `cleanroom network enable`
5. User approves the macOS system extension / Network Extension prompt.
6. User uses `cleanroom` normally.

### Day-to-day usage

- Normal usage stays in the terminal.
- The app is an implementation detail of `cleanroom network enable|disable|reset|status`.
- If launched manually, the app should show a short "use the CLI" message and exit.
- `cleanroom doctor` remains the source of truth for whether host-side filtering is actually active.

## Target Architecture

### Filter control

- `Cleanroom.app` activates the bundled Network Extension system extension via `OSSystemExtensionRequest`.
- `Cleanroom.app` then loads and saves `NEFilterManager` preferences.
- `cleanroom network install` installs `com.buildkite.cleanroom.network`.
- `cleanroom network enable|disable|reset|status` is the primary user-facing interface for filter control.
- `CleanroomFilterDataProvider` is bundled as a system extension and is the only filter provider shipped by this PR.
- The app must not claim success when preferences are enabled but the provider has not started.

### State handoff

- `cleanroom serve` publishes compiled filter policy and runtime identity to the root-owned network-filter daemon.
- The daemon persists policy and status under its own system state directory and exposes a localhost control API for the app and provider.
- The app updates filter-manager status through that daemon.
- The provider reads policy and publishes provider health through that daemon.
- Cleanroom must fail closed and report clearly when the provider is not actually active.

### Enforcement model

- The provider consumes compiled allow/deny policy, not repo YAML.
- Provider health feeds back into backend capability and doctor reporting.
- darwin-vz should only claim allowlist egress support when the provider is proven to be running.

## What This PR Should Deliver

This PR should land as one coherent change set with these properties:

- the macOS app is filter-only
- the CLI remains the canonical runtime manager
- the CLI is the canonical filter-control surface
- Network Extension signing and provisioning are documented and reproducible
- the bundled provider is packaged as a macOS system extension for self-distribution
- provider startup failure is observable from the app and from `cleanroom doctor`
- Cleanroom does not report egress allowlist support unless host-side enforcement is actually active

## Distribution Direction

Start with self-distribution.

Why:

- Network Extension packaging and signing are already strict.
- Cleanroom is primarily a CLI product.
- App Store distribution adds sandbox and review constraints without helping the core workflow.

The app should be treated as a signed macOS network utility, not the primary product surface.

## Out Of Scope For This PR

These are real follow-ups, but they should not be represented as staged rollout items inside this plan:

- vmnet-backed darwin-vz enforcement keyed by per-sandbox network identity
- packet-oriented filter provider work
- notarization and release hardening
- any broader settings UI beyond filter diagnostics
