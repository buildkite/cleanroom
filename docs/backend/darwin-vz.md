# Darwin VZ Backend

## Overview

`darwin-vz` is the macOS microVM backend for cleanroom. It uses a dedicated Swift helper binary (`cleanroom-darwin-vz`) for `Virtualization.framework` lifecycle operations and keeps policy/image/control-plane orchestration in Go.

This split means:

- Go owns policy validation, OCI/image preparation, kernel/rootfs selection, and command protocol semantics.
- Swift owns VM create/start/stop and guest transport bridging.

## Current Scope

Implemented:

- launched execution on macOS via `Virtualization.framework`
- interactive and non-interactive command execution via existing `internal/vsockexec` protocol
- helper-managed VM lifecycle (`StartVM` / `StopVM` / `PauseVM` / `ResumeVM`)
- `vmnet-shared` network mode on macOS 26+ with optional experimental custom RFC1918 subnet
- managed kernel fallback when `kernel_image` is unset or missing
- rootfs derivation from `sandbox.image.ref` when `rootfs` is unset or missing
- persistent sandboxes across multiple executions
- `file` and `apfs` snapshot drivers for snapshot and create-from-snapshot flows
- doctor checks for helper availability and entitlement status

Not implemented:

- egress allowlist filtering for `sandbox.network.allow`
- host-visible per-sandbox guest IP / TAP identity

## Process and Transport Model

Each provisioned sandbox starts a dedicated helper process and VM that are reused across executions until sandbox termination.

Control plane:

- socket: `<run_dir>/vz-helper.sock`
- protocol: newline-delimited JSON request/response
- operations: `StartVM`, `StopVM`, `PauseVM`, `ResumeVM`, `Ping`

Data plane:

- socket: `<run_dir>/vz-proxy.sock`
- protocol: raw byte stream carrying existing `vsockexec` frames unchanged

High-level flow:

1. Go resolves kernel and rootfs paths during sandbox provisioning.
2. Go starts `cleanroom-darwin-vz --socket <sandbox_run_dir>/vz-helper.sock`.
3. Go sends `StartVM`.
4. Helper starts VM and binds a long-lived proxy socket.
5. Each execution dials the proxy socket and runs normal `vsockexec` request/stream protocol.
6. Go sends `StopVM` during sandbox teardown.

## Helper Request Schema

`StartVM` request fields:

- `kernel_path` absolute path to Linux kernel
- `rootfs_path` absolute path to sandbox-scoped ext4 rootfs copy
- `vcpus`, `memory_mib`, `guest_port`, `launch_seconds`
- `run_dir`
- `proxy_socket_path`
- `console_log_path`

`StartVM` response fields:

- `ok`
- `vm_id`
- `proxy_socket_path`
- optional `timing_ms.vm_ready`

`StopVM` request:

- `op=StopVM`
- optional `vm_id` (validated when provided)

`PauseVM` / `ResumeVM` request:

- `op=PauseVM` or `op=ResumeVM`
- optional `vm_id` (validated when provided)

## Kernel and RootFS Strategy

Kernel:

- if configured kernel exists, use it
- otherwise resolve and cache a managed kernel asset under XDG data paths

Rootfs:

- if configured rootfs exists, use it
- otherwise derive rootfs from `sandbox.image.ref` using image manager
- inject guest runtime (`cleanroom-guest-agent` and `/usr/sbin/cleanroom-init`) into a prepared cached rootfs image
- create a per-sandbox copy (`rootfs-persistent.ext4`) and attach it read-write to the VM

Snapshot/rootfs volume drivers:

- omitted `snapshots.driver` defaults to `apfs` for `darwin-vz`
- `snapshots.driver: file` copies ext4 images with standard file I/O
- `snapshots.driver: apfs` uses macOS `clonefile(2)` for same-filesystem APFS copy-on-write clones and falls back to standard file copies when `clonefile(2)` is unavailable for the selected source/destination
- the selected driver is used for snapshot capture, snapshot-backed sandbox creation, and writable per-execution/per-sandbox rootfs preparation

Host tools required for derivation/injection:

- `mkfs.ext4`
- `debugfs`

On macOS, cleanroom also probes common Homebrew `e2fsprogs` locations.

## Networking Semantics

`darwin-vz` currently supports only two policy shapes:

- `network.default: deny` keeps the existing policy shape; `network.allow` entries are ignored and produce a warning
- `network.default: allow` disables egress filtering entirely and produces a warning
- a virtual NIC is attached with `Virtualization.framework` NAT networking, so guest outbound networking is available

The backend currently has no allowlist egress enforcement equivalent to Linux Firecracker iptables rules.

At runtime, `darwin-vz` emits an explicit stderr warning for this so it is visible during `exec`/`console`.

This is not equivalent to Firecracker's network model:

- the host does not get a dedicated per-sandbox TAP device
- the host does not get a stable per-sandbox guest IP identity to key firewall or gateway policy on
- there is no general host-to-guest inbound network path today beyond the existing helper-mediated control/exec path

The current helper-managed NAT model is intentionally simpler, but it leaves `darwin-vz` behind Firecracker in network identity and enforcement. Planned vmnet-backed work is tracked in [../plans/darwin-vz-vmnet-mode.md](../plans/darwin-vz-vmnet-mode.md).

Experimental runtime config:

- `backends.darwin-vz.network.mode: vmnet-shared|nat`
- `backends.darwin-vz.network.subnet: 10.233.0.0/16` for custom vmnet shared-mode IPv4 ranges

`vmnet-shared` is now the default only when the host supports it and the
resolved helper declares the vmnet entitlement. Otherwise the implicit default
falls back to `nat`. Use explicit `vmnet-shared` only when you want an
unsupported host or unsigned helper to fail fast instead of degrading to `nat`.
`vmnet-shared` accepts only RFC1918 IPv4 CIDRs. For `vmnet-shared`, the helper
now mirrors the Apple `containerization` pattern: create the vmnet shared
network in-helper, disable vmnet DHCP, derive the actual subnet from vmnet, and
pass a static guest IPv4/gateway/prefix to the guest init path via boot args.

## Capability Surface

Backends now expose a machine-readable capability map (visible in `cleanroom doctor --json` under `capabilities`).

Current `darwin-vz` capability values when snapshot support is enabled:

- `exec.streaming=true`
- `sandbox.persistent=true`
- `sandbox.snapshot=true`
- `sandbox.file_download=false`
- `network.default_deny=true`
- `network.allowlist_egress=false`
- `network.guest_interface=true`

Git traffic for allowed HTTPS hosts is routed through the host gateway over the
NAT host address. The default gateway host is `192.168.64.1`; override it for
unusual host networking setups with `CLEANROOM_DARWIN_GATEWAY_HOST`. Because
there is still no Firecracker-style source-IP identity in this mode, gateway
scoping currently relies on helper-managed scope-token headers rather than
Firecracker-style source-IP identity.

## Entitlements and Signing

`cleanroom-darwin-vz` must include:

- `com.apple.security.virtualization`

The default `vmnet-shared` path additionally requires:

- `com.apple.developer.networking.vmnet`
- a matching provisioning profile for the helper identifier

The main `cleanroom` Go binary does not require this entitlement for `darwin-vz`.

`scripts/build-darwin-vz-helper.sh` is the canonical helper build/sign path. By
default it emits a signed `cleanroom-darwin-vz.app` bundle. When
`CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE` is set it embeds that profile in
the bundle so the helper can carry restricted entitlements. Set
`CLEANROOM_DARWIN_VZ_HELPER_BUNDLE=0` to emit a loose helper binary instead.
Without an explicit output path, the helper is staged at
`dist/<host-goos>-<host-goarch>/libexec/cleanroom/cleanroom-darwin-vz.app`.

When a prebuilt helper `.app` bundle is available, the install script preserves
that bundle as-is and only re-signs it when the caller explicitly provides
helper signing overrides. Re-signing without a replacement provisioning profile
drops any embedded source profile and falls back to the plain virtualization
entitlements by default.

Relevant env vars:

- `CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS`
- `CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY`
- `CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER` (defaults to `com.buildkite.cleanroom.darwin-vz` when a provisioning profile is embedded)
- `CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE`

Known-good local setup for vmnet-shared:

1. Enable the Apple `VMNet` capability for `com.buildkite.cleanroom.darwin-vz`.
2. Regenerate and install the matching macOS provisioning profile.
3. Keep the downloaded profile available locally, for example at `~/Downloads/Cleanroom_Darwin_VZ_Backend.provisionprofile`.
4. Build the helper app with that embedded profile:

```bash
CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS=cmd/cleanroom-darwin-vz/entitlements-vmnet.plist \
CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY='Apple Development: <you> (<team>)' \
CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER='com.buildkite.cleanroom.darwin-vz' \
CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE="$HOME/Downloads/Cleanroom_Darwin_VZ_Backend.provisionprofile" \
scripts/build-darwin-vz-helper.sh
```

## Runtime Discovery

The helper path is resolved in this order:

1. `CLEANROOM_DARWIN_VZ_HELPER`
2. installed `../libexec/cleanroom/cleanroom-darwin-vz.app` relative to `cleanroom`
3. sibling binary next to `cleanroom`
4. sibling `cleanroom-darwin-vz.app` bundle next to `cleanroom`
5. `dist/<host-goos>-<host-goarch>/libexec/cleanroom/cleanroom-darwin-vz.app` under the current working directory or one of its ancestors
6. legacy `dist/cleanroom-darwin-vz.app` or `dist/cleanroom-darwin-vz` under the current working directory or one of its ancestors
7. `PATH`

If missing, runtime fails with an actionable error.

The Linux guest agent follows the same general pattern:

1. installed `../libexec/cleanroom/cleanroom-guest-agent-linux-$GOARCH` relative to `cleanroom`
2. sibling binary next to `cleanroom`
3. `dist/<host-goos>-<host-goarch>/libexec/cleanroom/cleanroom-guest-agent-linux-$GOARCH` under the current working directory or one of its ancestors
4. legacy `dist/cleanroom-guest-agent-linux-$GOARCH` under the current working directory or one of its ancestors
5. `PATH`

`mise run build` now produces the matching staged runtime under `dist/<host-goos>-<host-goarch>/` for macOS development.

## Testing

Fast path:

- `mise exec -- go test ./internal/backend/darwinvz`
- `mise exec -- go test ./...`
- `mise exec -- go test -run '^$' -bench BenchmarkDarwinSnapshotDrivers -benchmem -benchtime=5x ./internal/volumestore`

Real VM persistence e2e:

1. Build the matching prebuilt binaries: `mise run build`
2. If you are not running from the checkout that contains `dist/`, ensure the linux guest agent is installed or otherwise discoverable.
3. If you are not providing `CLEANROOM_DARWIN_VZ_E2E_ROOTFS`, install `e2fsprogs` on macOS so `mkfs.ext4` and `debugfs` are available.
4. Run the opt-in test:

```bash
CLEANROOM_DARWIN_VZ_E2E=1 \
CLEANROOM_DARWIN_VZ_E2E_IMAGE_REF="docker.io/library/alpine@sha256:a4f4213abb84c497377b8544c81b3564f313746700372ec4fe84653e4fb03805" \
mise exec -- go test ./internal/backend/darwinvz -run TestPersistentSandboxE2E -v
```

Supported e2e overrides:

- `CLEANROOM_DARWIN_VZ_E2E_IMAGE_REF` selects the sandbox image ref used for rootfs derivation.
- `CLEANROOM_DARWIN_VZ_E2E_KERNEL_IMAGE` forces a specific kernel path instead of the managed kernel asset.
- `CLEANROOM_DARWIN_VZ_E2E_ROOTFS` points at a prebuilt ext4 rootfs and skips the host `e2fsprogs` requirement.

Focused vmnet spike path:

```bash
CLEANROOM_DARWIN_VZ_HELPER="$PWD/dist/$(go env GOOS)-$(go env GOARCH)/libexec/cleanroom/cleanroom-darwin-vz.app" \
CLEANROOM_DARWIN_VZ_VMNET_E2E=1 \
mise exec -- go test ./internal/backend/darwinvz -run TestVMNetSharedE2E -v
```

The vmnet e2e expects the helper to already be signed with
`cmd/cleanroom-darwin-vz/entitlements-vmnet.plist` and a provisioning profile
that actually grants `com.apple.developer.networking.vmnet`.

The current automated vmnet path covers:

- `vmnet-shared` on the default shared network
- guest IPv4 assignment via helper-provided static config
- custom RFC1918 shared subnet egress, currently exercised with `10.233.0.0/16`
- host-to-guest TCP reachability on the custom subnet path
- guest outbound egress

The host-to-guest check builds a tiny Linux guest test binary from
`cmd/cleanroom-vmnet-echo`, injects it into a temporary e2e rootfs, runs it
inside the guest, then verifies the host can connect to the guest IP directly.

Important implementation detail:

- when configuring a custom subnet, the helper must pass the host/gateway
  address to `vmnet_network_configuration_set_ipv4_subnet`, not the raw network
  address. Apple’s `containerization` code does this, and matching that
  behavior fixed the previous custom-subnet bring-up failure in this repo.

Known limits from the macOS 26.1 spike:

- identity persistence across restarts is still not proven
- guest metadata is now persisted in darwin-vz config/observability files, but
  it is still derived per launch rather than allocated from a longer-lived
  identity model

## Limitations

- no allowlist egress filtering yet
- no sandbox file download support yet
- no Firecracker-style TAP identity or host firewall enforcement yet
- direct host-to-guest routing is only exercised on the experimental custom
  subnet vmnet path
