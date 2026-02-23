# 🧑‍🔬 Cleanroom

Cleanroom uses fast Firecracker microVMs (under 2s to interactive) to create isolated environments for untrusted workloads like AI agents and CI jobs. Deny-by-default egress, digest-pinned images, and immutable policy make it safe to run code you don't trust.

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
- Transport: unix socket (default), HTTPS with mTLS, or Tailscale
- Core RPC services:
  - `cleanroom.v1.SandboxService`
  - `cleanroom.v1.ExecutionService`

## Quick Start

Start the server (all CLI commands require a running server):

```bash
cleanroom serve &
```

The server listens on `unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock` by default.

Run a command in a sandbox:

```bash
cleanroom exec -- npm test
```

The sandbox stays running after the command completes. Run more commands against it:

```bash
cleanroom exec --sandbox-id <id> -- npm run lint
cleanroom exec --sandbox-id <id> -- npm run build
```

Use `--rm` to tear down the sandbox after the command completes (useful for one-off CI jobs):

```bash
cleanroom exec --rm -- npm test
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
- Rootfs writes persist across executions within a sandbox and are discarded on sandbox termination
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
