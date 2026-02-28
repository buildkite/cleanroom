# 🧑‍🔬 Cleanroom

Cleanroom uses fast Linux microVM backends to create isolated environments for untrusted workloads like AI agents and CI jobs. It supports Firecracker on Linux and Virtualization.framework (`darwin-vz`) on macOS. Deny-by-default policy, digest-pinned images, and immutable policy compilation make it safe to run code you don't trust.

## What It Does

- Compiles repository policy from `cleanroom.yaml`
- Requires digest-pinned sandbox base images via `sandbox.image.ref`
- Enforces deny-by-default egress (`host:port` allow rules)
- Runs commands in a backend microVM (`firecracker` on Linux, `darwin-vz` on macOS)
- Returns exit code + stdout/stderr over API
- Stores run artifacts and timing metrics for inspection

## Backend Support

| Host OS | Backend | Status | Notes |
|---------|---------|--------|-------|
| Linux | `firecracker` | Full local backend | Supports persistent sandboxes, sandbox file download, and egress allowlist enforcement |
| macOS | `darwin-vz` | Supported with known gaps | Per-run VMs (no persistent sandboxes yet), no sandbox file download, guest NIC with unfiltered egress, and no egress allowlist enforcement yet |

Backend capabilities are exposed in `cleanroom doctor --json` under `capabilities`.

## Architecture

- Server: `cleanroom serve`
- Client: CLI and ConnectRPC clients
- Transport: unix socket (default), HTTPS with mTLS, or Tailscale
- Core RPC services:
  - `cleanroom.v1.SandboxService`
  - `cleanroom.v1.ExecutionService`

## Quick Start

Initialize runtime config and check host prerequisites:

```bash
cleanroom config init
cleanroom doctor
```

Start the server (all CLI commands require a running server):

```bash
cleanroom serve &
```

The server listens on `unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock` by default.

Run a command in a sandbox:

```bash
cleanroom exec -- npm test
```

The sandbox stays running after the command completes. List sandboxes and run more commands:

```bash
cleanroom sandbox ls
cleanroom exec --sandbox-id <id> -- npm run lint
cleanroom exec --sandbox-id <id> -- npm run build
```

Use `--rm` to tear down the sandbox after the command completes (useful for one-off CI jobs):

```bash
cleanroom exec --rm -- npm test
```

macOS note:

- `darwin-vz` is the default backend on macOS
- install host tools for rootfs derivation with `brew install e2fsprogs`
- `cleanroom-darwin-vz` helper must be installed and signed with `com.apple.security.virtualization` entitlement (`mise run install` handles this in this repo)

## CLI

```bash
cleanroom exec -- npm test
cleanroom exec -c /path/to/repo -- make build
cleanroom exec --backend darwin-vz -- npm test
cleanroom exec --backend firecracker -- npm test
```

Interactive console:

```bash
cleanroom console -- bash
```

## TLS / mTLS

Cleanroom supports mutual TLS for HTTPS transport. TLS material is stored in
`$XDG_CONFIG_HOME/cleanroom/tls/` (typically `~/.config/cleanroom/tls/`).

### Bootstrap certificates

```bash
cleanroom tls init
```

This generates a CA, server certificate (with localhost + hostname SANs), and
client certificate. Use `--force` to overwrite existing material.

### Issue additional certificates

```bash
cleanroom tls issue worker-1 --san worker-1.internal --san 10.0.0.5
```

When `--san` is omitted, the certificate name is added as a SAN automatically.

### Serve with HTTPS + mTLS

```bash
cleanroom serve --listen https://0.0.0.0:7777
```

TLS material is auto-discovered from the XDG TLS directory. To use explicit
paths:

```bash
cleanroom serve --listen https://0.0.0.0:7777 \
  --tls-cert /path/to/server.pem \
  --tls-key /path/to/server.key \
  --tls-ca /path/to/ca.pem
```

When a CA is configured, the server requires and verifies client certificates
(mTLS). Without `--tls-ca`, the server accepts any TLS client.

### Connect over HTTPS

```bash
cleanroom exec --host https://server.example.com:7777 -- echo hello
```

Client certificates and CA are auto-discovered from the XDG TLS directory, or
specified with `--tls-cert`, `--tls-key`, and `--tls-ca`.

Environment variables `CLEANROOM_TLS_CERT`, `CLEANROOM_TLS_KEY`, and
`CLEANROOM_TLS_CA` are also supported.

### Auto-discovery

When no explicit TLS flags are provided, cleanroom looks for:

| Role   | Cert                | Key                 | CA       |
|--------|---------------------|---------------------|----------|
| Server | `<tlsdir>/server.pem` | `<tlsdir>/server.key` | `<tlsdir>/ca.pem` |
| Client | `<tlsdir>/client.pem` | `<tlsdir>/client.key` | `<tlsdir>/ca.pem` |

CA auto-discovery is skipped when cert/key are explicitly provided, to avoid
unexpectedly enabling mTLS.

TLS commands:

```bash
cleanroom tls init                    # generate CA + server/client certs
cleanroom tls issue myhost --san myhost.internal
```

## Image Lifecycle

```bash
cleanroom image pull ghcr.io/buildkite/cleanroom-base/alpine@sha256:...
cleanroom image ls
cleanroom image rm sha256:...
cleanroom image import ghcr.io/buildkite/cleanroom-base/alpine@sha256:... ./rootfs.tar.gz
cleanroom image bump-ref
```

`ghcr.io/buildkite/cleanroom-base/alpine` is published from this repo on pushes to `main`
via `.github/workflows/base-image.yml`.

Sandbox management:

```bash
cleanroom sandbox ls
cleanroom sandbox rm <sandbox-id>
```

Diagnostics:

```bash
cleanroom doctor
cleanroom status --run-id <run-id>
cleanroom status --last-run
```

## Remote Access

The server supports Tailscale listeners for remote access:

```bash
# Embedded tsnet
cleanroom serve --listen tsnet://cleanroom:7777
cleanroom exec --host http://cleanroom.tailnet.ts.net:7777 -c /path/to/repo -- npm test

# Tailscale Service (via local tailscaled)
cleanroom serve --listen tssvc://cleanroom
cleanroom exec --host https://cleanroom.<your-tailnet>.ts.net -- npm test
```

HTTP is also supported: `cleanroom serve --listen http://0.0.0.0:7777`

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

## Runtime Config

Runtime config path: `$XDG_CONFIG_HOME/cleanroom/config.yaml` (or `~/.config/cleanroom/config.yaml`).

Bootstrap a config file with defaults:

```bash
cleanroom config init
```

On macOS, `cleanroom config init` defaults `default_backend` to `darwin-vz`. On other platforms it defaults to `firecracker`.

```yaml
default_backend: firecracker
backends:
  firecracker:
    binary_path: firecracker
    kernel_image: "" # optional; auto-managed when unset/missing
    privileged_mode: sudo # or helper
    privileged_helper_path: /usr/local/sbin/cleanroom-root-helper
    vcpus: 2
    memory_mib: 1024
    guest_cid: 3
    launch_seconds: 30
  darwin-vz:
    kernel_image: "" # optional; auto-managed when unset/missing
    rootfs: "" # optional; derived from sandbox.image.ref when unset/missing
    vcpus: 2
    memory_mib: 1024
    guest_port: 10700
    launch_seconds: 30
```

The Firecracker adapter resolves `sandbox.image.ref` through the local image manager and caches materialised ext4 files under XDG cache paths.
When `backends.<name>.kernel_image` is unset (or points to a missing file), cleanroom auto-downloads a verified managed kernel asset into XDG data paths.
Set `kernel_image` explicitly if you need fully offline operation.
When `backends.<name>.rootfs` is unset (or points to a missing file), cleanroom derives a runtime rootfs from `sandbox.image.ref` and injects the guest runtime automatically.
This derivation path requires `mkfs.ext4` and `debugfs` on the host.
On macOS, cleanroom auto-detects Homebrew `e2fsprogs` (`mkfs.ext4`/`debugfs`) from common keg locations even when they are not in `PATH`.
The `darwin-vz` backend launches a dedicated helper binary (`cleanroom-darwin-vz`) and resolves it in this order:
1. `CLEANROOM_DARWIN_VZ_HELPER`
2. sibling binary next to `cleanroom`
3. `PATH`

The helper (not the main `cleanroom` binary) needs the `com.apple.security.virtualization` entitlement.

## Isolation Model

- Workload runs in a Linux microVM backend (`firecracker` on Linux, `darwin-vz` on macOS)
- `firecracker` enforces policy egress allowlists with TAP + iptables
- `darwin-vz` currently requires `network.default: deny`, ignores `network.allow` entries, and provides guest networking without egress filtering (warns on stderr during execution)
- `firecracker` rootfs writes persist across executions within a sandbox and are discarded on sandbox termination
- `darwin-vz` executes each command in a fresh VM/rootfs copy (writes are discarded after each run)
- Per-run observability is written to `run-observability.json` (rootfs prep, network setup, VM ready, command runtime, total)
- `firecracker` rootfs copy uses clone/reflink when available, with copy fallback

## Host Requirements

Linux (`firecracker` backend):

- `/dev/kvm` available and writable
- Firecracker binary installed
- kernel image configured, or internet access for first-run managed kernel download
- `mkfs.ext4` installed (materialises OCI layers into ext4 cache artifacts)
- `sudo -n` access for `ip`, `iptables`, and `sysctl` (network setup/cleanup)

macOS (`darwin-vz` backend):

- `cleanroom-darwin-vz` helper installed
- helper binary signed with `com.apple.security.virtualization` entitlement
- `mkfs.ext4` and `debugfs` for rootfs derivation (`brew install e2fsprogs`)

General:

- `cleanroom doctor --json` includes a machine-readable `capabilities` map for the selected backend

## References

- [API design](docs/api.md) — ConnectRPC surface and proto sketch
- [Benchmarks](docs/benchmarks.md) — TTI measurement and results
- [CI](docs/ci.md) — Buildkite pipeline and base image workflow
- [Darwin VZ](docs/darwin-vz.md) — macOS backend/helper design and behavior
- [Spec](docs/spec.md) — Full specification and roadmap
- [Research](docs/research.md) — Backend and tooling evaluation notes
