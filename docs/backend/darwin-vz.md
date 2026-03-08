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
- persistent sandboxes across multiple executions
- doctor checks for helper availability and entitlement status

Not implemented:

- egress allowlist filtering for `sandbox.network.allow`

## Process and Transport Model

Each provisioned sandbox starts a dedicated helper process and VM that are reused across executions until sandbox termination.

Control plane:

- socket: `<run_dir>/vz-helper.sock`
- protocol: newline-delimited JSON request/response
- operations: `StartVM`, `StopVM`, `Ping`

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

## Kernel and RootFS Strategy

Kernel:

- if configured kernel exists, use it
- otherwise resolve and cache a managed kernel asset under XDG data paths

Rootfs:

- if configured rootfs exists, use it
- otherwise derive rootfs from `sandbox.image.ref` using image manager
- inject guest runtime (`cleanroom-guest-agent` and `/sbin/cleanroom-init`) into a prepared cached rootfs image
- create a per-sandbox copy (`rootfs-persistent.ext4`) and attach it read-write to the VM

Host tools required for derivation/injection:

- `mkfs.ext4`
- `debugfs`

On macOS, cleanroom also probes common Homebrew `e2fsprogs` locations.

## Networking Semantics

`darwin-vz` currently enforces only deny-by-default policy shape:

- `network.default` must be `deny`
- `network.allow` entries are ignored and produce a warning
- a virtual NIC is attached (NAT), so guest outbound networking is available

The backend currently has no allowlist egress enforcement equivalent to Linux Firecracker iptables rules.

At runtime, `darwin-vz` emits an explicit stderr warning for this so it is visible during `exec`/`console`.

## Capability Surface

Backends now expose a machine-readable capability map (visible in `cleanroom doctor --json` under `capabilities`).

Current `darwin-vz` capability values:

- `exec.streaming=true`
- `sandbox.persistent=true`
- `sandbox.file_download=false`
- `network.default_deny=true`
- `network.allowlist_egress=false`
- `network.guest_interface=true`

Gateway access for git rewrite flow:

- darwin guests can access the host gateway through the NAT host address
- default host is `192.168.64.1`; override with `CLEANROOM_DARWIN_GATEWAY_HOST`

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

## Testing

Fast path:

- `mise exec -- go test ./internal/backend/darwinvz`
- `mise exec -- go test ./...`

Real VM persistence e2e:

1. Build the signed helper: `mise run build:darwin`
2. Ensure the linux guest agent is installed or otherwise discoverable.
3. If you are not providing `CLEANROOM_DARWIN_VZ_E2E_ROOTFS`, install `e2fsprogs` on macOS so `mkfs.ext4` and `debugfs` are available.
4. Run the opt-in test:

```bash
CLEANROOM_DARWIN_VZ_E2E=1 \
CLEANROOM_DARWIN_VZ_HELPER="$PWD/dist/cleanroom-darwin-vz" \
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
