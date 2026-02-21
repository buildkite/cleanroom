# Cleanroom

Cleanroom is an API-first control plane for isolated command execution.

Current backend: **Firecracker** on Linux/KVM.

It is built for this lifecycle:
1. Launch a cleanroom context from repository policy.
2. Run an agent/command in an isolated Firecracker VM.
3. Terminate the cleanroom context.

## What It Does

- Compiles repository policy from `cleanroom.yaml`
- Requires digest-pinned sandbox base images via `sandbox.image.ref`
- Enforces deny-by-default egress (`host:port` allow rules)
- Runs commands in a Firecracker microVM via guest agent
- Returns exit code + stdout/stderr over API
- Stores run artifacts and timing metrics for inspection

## Architecture

- Server: `cleanroom serve`
- Client: CLI and ConnectRPC clients
- Transport: unix socket by default (`unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock`)
- Core RPC services:
  - `cleanroom.v1.SandboxService`
  - `cleanroom.v1.ExecutionService`

## Quick Start

### 1) Validate policy

```bash
cleanroom policy validate
```

### 2) Start API server

```bash
cleanroom serve --listen unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock
```

All CLI commands (including `cleanroom exec`) talk to this API endpoint.

To expose the API over Tailscale using embedded `tsnet`:

```bash
cleanroom serve --listen tsnet://cleanroom:7777
```

Then connect with:

```bash
cleanroom exec --host http://cleanroom.tailnet.ts.net:7777 -c /path/to/repo -- "npm test"
```

To expose the API as a Tailscale Service using the local `tailscaled` daemon:

```bash
cleanroom serve --listen tssvc://cleanroom
```

This configures `svc:cleanroom` with HTTPS on port 443 and advertises the
service from this host. Connect from another tailnet device with:

```bash
cleanroom exec --host https://cleanroom.<your-tailnet>.ts.net -- "npm test"
```

### 3) Launch -> Run -> Terminate via API

Use the generated ConnectRPC clients against `SandboxService` and
`ExecutionService` for lifecycle and execution operations.

## CLI Shortcut

`cleanroom exec` keeps a one-command flow, but lifecycle endpoints are the primary API model.

```bash
cleanroom exec -- pi run "implement the requested refactor"
```

Image lifecycle commands:

```bash
cleanroom image pull ghcr.io/buildkite/cleanroom-base/alpine@sha256:...
cleanroom image ls
cleanroom image rm sha256:...
cleanroom image import ghcr.io/buildkite/cleanroom-base/alpine@sha256:... ./rootfs.tar.gz
cleanroom image set-ref ghcr.io/buildkite/cleanroom-base/alpine@sha256:...
```

`ghcr.io/buildkite/cleanroom-base/alpine` is published from this repo on pushes to `main`
via `.github/workflows/base-image.yml`.

## API Contract (Current)

Canonical API surface is defined in `proto/cleanroom/v1/control.proto` and
served over ConnectRPC.

## Policy File

Repository policy path resolution:
1. `cleanroom.yaml`
2. `.buildkite/cleanroom.yaml` (fallback)

Minimal example:

```yaml
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny
    allow:
      - host: api.github.com
        ports: [443]
      - host: registry.npmjs.org
        ports: [443]
```

## Firecracker Runtime Config

Runtime config path: `$XDG_CONFIG_HOME/cleanroom/config.yaml` (or `~/.config/cleanroom/config.yaml`).

Example:

```yaml
default_backend: firecracker
backends:
  firecracker:
    binary_path: firecracker
    kernel_image: /opt/cleanroom/vmlinux
    privileged_mode: sudo # or helper
    privileged_helper_path: /usr/local/sbin/cleanroom-root-helper
    vcpus: 2
    memory_mib: 1024
    guest_cid: 3
    launch_seconds: 30
```

The Firecracker adapter resolves `sandbox.image.ref` through the local image manager and caches materialised ext4 files under XDG cache paths.

## Isolation Model

- Workload runs in a Firecracker microVM
- Host egress is controlled with TAP + iptables rules from compiled policy
- Default network behavior is deny
- Rootfs writes are discarded after each run

## Performance Notes

- Rootfs copy uses clone/reflink when available, with copy fallback
- Per-run observability is written to `run-observability.json`
- Metrics include:
  - rootfs preparation time
  - network setup time
  - VM ready time
  - guest command runtime
  - total run time

Inspect:

```bash
cleanroom doctor
cleanroom status --run-id <run-id>
```

`status --run-id` prints per-run observability from `run-observability.json`, including:
- policy host-resolution time
- rootfs preparation time
- Firecracker process start time
- network setup time
- VM ready time (process start -> guest agent ready)
- vsock wait time (wait-to-connect for guest agent)
- guest command runtime
- network cleanup time
- total run time

## Host Requirements

- Linux host
- `/dev/kvm` available and writable
- Firecracker binary installed
- Kernel image configured
- `mkfs.ext4` installed (used to materialise OCI layers into ext4 cache artifacts)
- `sudo -n` access for `ip`, `iptables`, and `sysctl` (network setup/cleanup)

## References

- API design notes: `API_CONNECTRPC.md`
- Full spec and roadmap: `SPEC.md`
