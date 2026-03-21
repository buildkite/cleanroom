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

`darwin-vz` currently enforces only deny-by-default policy shape:

- `network.default` must be `deny`
- `network.allow` entries are ignored and produce a warning
- a virtual NIC is attached with `Virtualization.framework` NAT networking, so guest outbound networking is available

The backend currently has no allowlist egress enforcement equivalent to Linux Firecracker iptables rules.

At runtime, `darwin-vz` emits an explicit stderr warning for this so it is visible during `exec`/`console`.

This is not equivalent to Firecracker's network model:

- the host does not get a dedicated per-sandbox TAP device
- the host does not get a stable per-sandbox guest IP identity to key firewall or gateway policy on
- there is no general host-to-guest inbound network path today beyond the existing helper-mediated control/exec path

The current helper-managed NAT model is intentionally simpler, but it leaves `darwin-vz` behind Firecracker in network identity and enforcement. Planned vmnet-backed work is tracked in [../plans/darwin-vz-vmnet-mode.md](../plans/darwin-vz-vmnet-mode.md).

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
the host lacks a stable per-sandbox guest IP identity in this mode, gateway
scoping currently relies on helper-managed scope-token headers rather than
Firecracker-style source-IP identity.

## Entitlements and Signing

`cleanroom-darwin-vz` must include:

- `com.apple.security.virtualization`

The main `cleanroom` Go binary does not require this entitlement for `darwin-vz`.

`mise run build:darwin` and `mise run install:darwin` both sign the helper with `cmd/cleanroom-darwin-vz/entitlements.plist`.

## Runtime Discovery

The helper path is resolved in this order:

1. `CLEANROOM_DARWIN_VZ_HELPER`
2. sibling binary next to `cleanroom`
3. `dist/` under the current working directory or one of its ancestors
4. `PATH`

If missing, runtime fails with an actionable error.

The Linux guest agent follows the same general pattern:

1. sibling binary next to `cleanroom`
2. `dist/cleanroom-guest-agent-linux-$GOARCH` under the current working directory or one of its ancestors
3. `PATH`

`mise run build` now produces the matching prebuilt set in `dist/` for macOS development.

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

The e2e test provisions one sandbox, runs two commands against it, asserts that filesystem writes survive across executions, then terminates the sandbox and verifies runtime cleanup.

## Limitations

- no allowlist egress filtering yet
- no sandbox file download support yet
- no host-visible per-sandbox guest IP / TAP identity yet
