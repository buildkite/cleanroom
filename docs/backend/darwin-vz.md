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
- helper-managed VM lifecycle (`StartVM` / `StopVM` / `PauseVM` / `ResumeVM` / `SetMemoryBalloonTarget`)
- `filehandle` network mode with a Cleanroom-owned guest gateway and stable guest IP
- TCP allowlist egress filtering for the active effective policy in `filehandle` mode
- allow-all egress for repo-agnostic sandboxes created with `cleanroom sandbox create --dangerously-allow-all`
- hostname-based allow rules currently use observed DNS answers plus destination IP:port, so co-hosted services on the same IP:port are not distinguished
- guest access to the shared host gateway through the stable hostname `gateway.cleanroom.internal`
- managed kernel fallback when `kernel_image` is unset or missing
- rootfs derivation from `sandbox.image.ref` when `rootfs` is unset or missing
- persistent sandboxes across multiple executions
- `file` and `apfs` snapshot drivers for snapshot and create-from-snapshot flows
- doctor checks for helper availability and entitlement status

Not implemented:

- host port publishing / additional stable hostnames such as `*.cleanroom.internal`
- general UDP/IPv6 allowlist policy beyond the current DNS + TCP filehandle path

## Process and Transport Model

Each provisioned sandbox starts a dedicated helper process and VM that are reused across executions until sandbox termination.

Control plane:

- socket: `<run_dir>/vz-helper.sock`
- protocol: newline-delimited JSON request/response
- operations: `StartVM`, `StopVM`, `PauseVM`, `ResumeVM`, `SetMemoryBalloonTarget`, `Ping`

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
- optional `initial_memory_balloon_target_mib`
- `run_dir`
- `proxy_socket_path`
- `console_log_path`

`vcpus` and `memory_mib` are effective VM launch ceilings after runtime config
and policy resource minimums are merged. They are not an exact host reservation
contract.

When `memory_mib` is above 1024 MiB, Go sends `initial_memory_balloon_target_mib=1024`
so the helper can request a smaller guest memory target during boot. After the
VM starts, Go sends `SetMemoryBalloonTarget` back to `memory_mib` before the
guest readiness probe, then re-sends the target before workload execution
without adding a fixed settle delay before exec requests. This keeps the
user-facing memory contract as a ceiling while allowing the backend to optimize
boot-time host memory pressure internally without adding unconditional latency
to tiny commands.

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

`SetMemoryBalloonTarget` request:

- `op=SetMemoryBalloonTarget`
- optional `vm_id` (validated when provided)
- `memory_balloon_target_mib` target guest memory allowance in MiB, capped by `memory_mib`

## Kernel and RootFS Strategy

Kernel:

- if configured kernel exists, use it
- otherwise resolve and cache a managed kernel asset under XDG data paths
- `backends.darwin-vz.kernel_image` is an explicit local kernel path override

To build the experimental minimal rootfs-profile kernel locally:

```bash
mise run build:kernel:darwin-vz-minimal-rootfs
```

Then point runtime config at the generated kernel:

```yaml
backends:
  darwin-vz:
    kernel_image: /path/to/cleanroom/dist/darwin-vz-minimal-rootfs-arm64-kernel-6.1.155-Image
```

Use `kernel_image` for local kernel experiments. It always wins when the path is
configured and accessible.

Tagged releases also publish the rootfs-profile kernel as direct GitHub Release
assets. The manifest is named with this pattern:

```text
cleanroom-darwin-vz-minimal-rootfs-arm64-linux-<linux-version>.manifest.json
```

The manifest names the matching `Image`, `.config`, and `.sha256` assets and
contains the expected image digest.

When `kernel_image` is not configured, `darwin-vz` resolves the managed kernel
from GitHub Releases:

- released Cleanroom builds use the matching Cleanroom release tag
- dev, dirty, and non-release builds use the latest published Cleanroom release
- if the release manifest is not available yet, Cleanroom falls back to the
  older managed kernel asset

To test an unreleased local kernel, build it locally and point `kernel_image` at
the generated `Image`.

Rootfs:

- if configured rootfs exists, use it
- otherwise derive rootfs from `sandbox.image.ref` using image manager
- inject guest runtime (`cleanroom-guest-agent` and `/usr/sbin/cleanroom-init`) into a prepared cached rootfs image
- create a per-sandbox copy (`rootfs-persistent.ext4`) and attach it read-write to the VM
- `/workspace` is not a separate volume on `darwin-vz`; it lives on the guest rootfs
- `backends.darwin-vz.minimum_rootfs_bytes` is an operator-wide writable rootfs floor; policy `sandbox.resources.disk` raises this floor when it is larger
- `minimum_rootfs_bytes` accepts either raw bytes (`2147483648`) or human-friendly sizes (`2GiB`, `700MiB`, `"2147483648"`)
- the same setting is also honored under the legacy underscore config key: `backends.darwin_vz.minimum_rootfs_bytes`
- image-derived prepared runtime rootfs cache entries are shared by image digest and guest runtime version; writable disk floors apply later to the per-sandbox rootfs copy
- see [Storage](../storage.md) for the storage model, sizing sources, and diagnostics
- effective vCPUs are fixed at VM launch; `darwin-vz` does not hotplug CPUs into a running VM

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

`darwin-vz` uses a single networking model:

- `filehandle`
  - attaches the guest NIC with `VZFileHandleNetworkDeviceAttachment`
  - gives each sandbox a stable private guest IP
  - runs a Cleanroom-owned guest gateway on the gateway IP, typically `10.233.0.1`
  - serves guest DNS from that gateway IP
  - enforces the active effective policy for TCP egress in the gateway
  - swaps the active policy before each sandbox command when stage-local network
    blocks are configured
  - bypasses egress filtering when the compiled policy uses `network.default=allow`
  - closes active TCP proxy connections when the active policy changes
  - authorizes hostname rules from observed DNS answers plus destination IP:port rather than HTTP `Host` or TLS SNI
  - exposes the shared host gateway service to the guest at `gateway.cleanroom.internal`

Compared to Firecracker:

- `filehandle` gives `darwin-vz` stable guest identity and host-owned filtering semantics
- host-to-guest networking beyond the guest gateway path and helper-mediated control/exec path is still not implemented

Experimental runtime config:

- `backends.darwin-vz.network.mode: filehandle`
- `backends.darwin-vz.network.subnet: 10.233.0.0/24` for custom filehandle private ranges

`filehandle` is the default and only supported `darwin-vz` network mode because
it is the only mode that matches Cleanroom's allowlisted egress semantics.

## Capability Surface

Backends now expose a machine-readable capability map (visible in `cleanroom doctor --json` under `capabilities`).

Current `darwin-vz` capability values:

- `exec.streaming=true`
- `sandbox.snapshot=true`
- `sandbox.file_download=true`
- `sandbox.file_upload=true`
- `sandbox.path_stat=true`
- `sandbox.tree_walk=true`
- `sandbox.file_read=true`
- `sandbox.file_write=true`
- `sandbox.path_remove=true`
- `sandbox.archive_read=true`
- `sandbox.archive_write=true`
- `network.default_deny=true`
- `network.allowlist_egress=true`
- `network.stage_scoped_egress=true`
- `network.guest_interface=true`

Git traffic for allowed HTTPS hosts uses the filehandle gateway path:

- `filehandle`
  - the guest rewrites allowed HTTPS Git remotes through the shared host gateway using `gateway.cleanroom.internal`
  - the guest does not need direct scope-token headers; the host bridge injects them

## Entitlements and Signing

`cleanroom-darwin-vz` must include:

- `com.apple.security.virtualization`

The main `cleanroom` Go binary does not require this entitlement for `darwin-vz`.

`scripts/build-darwin-vz-helper.sh` is the canonical helper build/sign path. By
default it emits a signed `cleanroom-darwin-vz.app` bundle. When
`CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE` is set it embeds that profile in
the bundle so the helper can carry restricted entitlements. Set
`CLEANROOM_DARWIN_VZ_HELPER_BUNDLE=0` to emit a loose helper binary instead.

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

## Runtime Discovery

The helper path is resolved in this order:

1. `CLEANROOM_DARWIN_VZ_HELPER`
2. sibling binary next to `cleanroom`
3. sibling `cleanroom-darwin-vz.app` bundle next to `cleanroom`
4. `dist/` under the current working directory or one of its ancestors
5. `dist/cleanroom-darwin-vz.app` under the current working directory or one of its ancestors
6. `PATH`

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

## Limitations

- no Firecracker-style TAP identity or host firewall enforcement yet
