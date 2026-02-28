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
- helper-managed VM lifecycle (`StartVM` / `StopVM`)
- managed kernel fallback when `kernel_image` is unset or missing
- rootfs derivation from `sandbox.image.ref` when `rootfs` is unset or missing
- doctor checks for helper availability and entitlement status

Not implemented:

- egress allowlist filtering for `sandbox.network.allow`
- persistent `darwin-vz` sandboxes across multiple executions

## Process and Transport Model

Each launched execution starts a dedicated helper process.

Control plane:

- socket: `<run_dir>/vz-helper.sock`
- protocol: newline-delimited JSON request/response
- operations: `StartVM`, `StopVM`, `Ping`

Data plane:

- socket: `<run_dir>/vz-proxy.sock`
- protocol: raw byte stream carrying existing `vsockexec` frames unchanged

High-level flow:

1. Go resolves kernel and rootfs paths.
2. Go starts `cleanroom-darwin-vz --socket <run_dir>/vz-helper.sock`.
3. Go sends `StartVM`.
4. Helper starts VM and binds proxy socket.
5. Go dials proxy socket and runs normal `vsockexec` request/stream protocol.
6. Go sends `StopVM` during teardown.

## Helper Request Schema

`StartVM` request fields:

- `kernel_path` absolute path to Linux kernel
- `rootfs_path` absolute path to per-run ext4 rootfs copy
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

## Kernel and RootFS Strategy

Kernel:

- if configured kernel exists, use it
- otherwise resolve and cache a managed kernel asset under XDG data paths

Rootfs:

- if configured rootfs exists, use it
- otherwise derive rootfs from `sandbox.image.ref` using image manager
- inject guest runtime (`cleanroom-guest-agent` and `/sbin/cleanroom-init`) into a prepared cached rootfs image
- create a per-run copy (`rootfs-ephemeral.ext4`) and attach it read-write to the VM

Host tools required for derivation/injection:

- `mkfs.ext4`
- `debugfs`

On macOS, cleanroom also probes common Homebrew `e2fsprogs` locations.

## Networking Semantics

`darwin-vz` currently enforces only deny-by-default policy shape:

- `network.default` must be `deny`
- `network.allow` entries are ignored and produce a warning

The backend currently has no allowlist egress enforcement equivalent to Linux Firecracker iptables rules.

## Entitlements and Signing

`cleanroom-darwin-vz` must include:

- `com.apple.security.virtualization`

The main `cleanroom` Go binary does not require this entitlement for `darwin-vz`.

`mise run build:darwin` and `mise run install:darwin` both sign the helper with `cmd/cleanroom-darwin-vz/entitlements.plist`.

## Runtime Discovery

The helper path is resolved in this order:

1. `CLEANROOM_DARWIN_VZ_HELPER`
2. sibling binary next to `cleanroom`
3. `PATH`

If missing, runtime fails with an actionable error.

## Limitations

- no allowlist egress filtering yet
- per-run helper/VM lifecycle only (no long-lived helper daemon)
- no cross-execution mutable sandbox state on `darwin-vz`
