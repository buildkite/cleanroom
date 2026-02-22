# 🧑‍🔬 Cleanroom

Cleanroom uses fast Firecracker microVMs (under 2s to interactive) to create isolated environments for untrusted workloads like AI agents and CI jobs. Deny-by-default egress, digest-pinned images, and immutable policy make it safe to run code you don't trust.

## What It Does

- Compiles repository policy from `cleanroom.yaml`
- Requires digest-pinned sandbox base images via `sandbox.image.ref`
- Enforces deny-by-default egress (`host:port` allow rules)
- Runs commands in a Firecracker microVM via guest agent
- Returns exit code + stdout/stderr over API
- Stores run artifacts and timing metrics for inspection

## Quick Start

Start the server (all CLI commands require a running server):

```bash
cleanroom serve &
```

The server listens on `unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock` by default.

The simplest way to run a command is `cleanroom exec`, which creates an ephemeral sandbox, runs the command, and tears it down:

```bash
cleanroom exec -- npm test
```

For long-running sandboxes, create one explicitly and run multiple commands against it:

```bash
cleanroom exec --keep-sandbox -- npm test
# reuse the sandbox for another command
cleanroom exec --sandbox-id <id> -- npm run lint
# terminate when done
```

## CLI

```bash
cleanroom exec -- npm test
cleanroom exec -c /path/to/repo -- make build
```

Interactive console:

```bash
cleanroom console -- bash
```

Image lifecycle:

```bash
cleanroom image pull ghcr.io/buildkite/cleanroom-base/alpine@sha256:...
cleanroom image ls
cleanroom image rm sha256:...
cleanroom image import ghcr.io/buildkite/cleanroom-base/alpine@sha256:... ./rootfs.tar.gz
cleanroom image bump-ref
```

`ghcr.io/buildkite/cleanroom-base/alpine` is published from this repo on pushes to `main`
via `.github/workflows/base-image.yml`.

Diagnostics:

```bash
cleanroom doctor
cleanroom status --run-id <run-id>
cleanroom status --last-run
```

## Architecture

- Server: `cleanroom serve`
- Client: CLI and ConnectRPC clients
- Transport: unix socket by default
- API: `cleanroom.v1.SandboxService` and `cleanroom.v1.ExecutionService`
- Proto definition: `proto/cleanroom/v1/control.proto`

### Remote Access

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
- Default network behaviour is deny
- Rootfs writes are discarded after each run
- Per-run observability is written to `run-observability.json` (rootfs prep, network setup, VM ready, command runtime, total)
- Rootfs copy uses clone/reflink when available, with copy fallback

## Host Requirements

- Linux host with `/dev/kvm` available and writable
- Firecracker binary installed
- Kernel image configured
- `mkfs.ext4` installed (materialises OCI layers into ext4 cache artifacts)
- `sudo -n` access for `ip`, `iptables`, and `sysctl` (network setup/cleanup)

## References

- [API design](docs/api.md) — ConnectRPC surface and proto sketch
- [Benchmarks](docs/benchmarks.md) — TTI measurement and results
- [CI](docs/ci.md) — Buildkite pipeline and base image workflow
- [Spec](docs/spec.md) — Full specification and roadmap
- [Research](docs/research.md) — Backend and tooling evaluation notes
