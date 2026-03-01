# 👩‍🔬 Cleanroom

Cleanroom runs untrusted code in microVMs with deny-by-default network policy. It is self-hosted, enforces repository-scoped egress rules, and keeps credentials on the host side of the VM boundary.

Agent sandboxing tools are [proliferating fast](docs/research.md). Most focus on isolation alone. Cleanroom adds policy-controlled network access so you decide exactly what the sandbox can reach.

## Why Cleanroom?

**Deny-by-default egress.** A `cleanroom.yaml` policy file in your repo controls exactly which hosts the sandbox can reach. Everything else is blocked.

**MicroVM isolation.** Each sandbox is a hardware-virtualized microVM (Firecracker on Linux, Virtualization.framework on macOS), not a container. A VM boundary is stronger than namespaces, seccomp, or gVisor -- a kernel vulnerability in the guest doesn't compromise the host.

**Self-hosted.** Runs on your infrastructure. Your code and data never leave your machines.

**Credentials stay on the host.** A [host-side gateway](docs/gateway.md) proxies git clones and package fetches, injecting credentials on the upstream leg. Tokens never enter the sandbox.

**Standard OCI images.** Use any OCI image from any registry as your sandbox base. Digest-pinned in policy for reproducibility. No custom VM image format or vendor-specific base images. Same image works across backends.

**Docker inside the sandbox.** Enable a guest Docker daemon with a single policy flag (`services.docker.required: true`). Build and run containers inside the microVM.

**Coming soon:** package registry proxy with lockfile enforcement, Docker pull caching, content caching for hermetic offline builds, and structured audit logging. See the [spec](docs/spec.md) for the full roadmap.

## Go Client (Public API)

Use `github.com/buildkite/cleanroom/client` from external Go modules.

```go
import (
  "context"
  "os"

  "github.com/buildkite/cleanroom/client"
)

func example() error {
  c := client.Must(client.NewFromEnv())

  sb, err := c.EnsureSandbox(context.Background(), "thread:abc123", client.EnsureSandboxOptions{
    Backend: "firecracker",
    Policy: client.PolicyFromAllowlist(
      "ghcr.io/buildkite/cleanroom-base/alpine@sha256:...",
      "sha256:...",
      client.Allow("api.github.com", 443),
      client.Allow("registry.npmjs.org", 443),
    ),
  })
  if err != nil { return err }

  result, err := c.ExecAndWait(context.Background(), sb.ID, []string{"bash", "-lc", "echo hello"}, client.ExecOptions{
    Stdout: os.Stdout,
    Stderr: os.Stderr,
  })
  if err != nil { return err }
  _ = result
  return nil
}
```

`client` exposes:
- `client.Client` for RPC calls
- protobuf request/response/event types (for example `client.CreateExecutionRequest`)
- status enums (`client.SandboxStatus_*`, `client.ExecutionStatus_*`)
- ergonomic wrappers (`client.NewFromEnv`, `client.EnsureSandbox`, `client.ExecAndWait`)

## Install

Install the latest release:

```bash
curl -fsSL https://raw.githubusercontent.com/buildkite/cleanroom/main/scripts/install.sh | bash
```

Install a specific version:

```bash
curl -fsSL https://raw.githubusercontent.com/buildkite/cleanroom/main/scripts/install.sh | \
  bash -s -- --version v0.1.0
```

By default this installs to `/usr/local/bin`. Override with `--install-dir` or `CLEANROOM_INSTALL_DIR`.

## Quick start

Initialize runtime config and check host prerequisites:

```bash
cleanroom config init
cleanroom doctor
```

Start the server (all CLI commands need a running server):

```bash
cleanroom serve &
```

The server listens on `unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock` by default.

Install as a system daemon (Linux `systemd` / macOS `launchd`):

```bash
sudo cleanroom serve install
```

Use `--force` to overwrite an existing service file:

```bash
sudo cleanroom serve install --force
```

The system daemon socket is root-owned (`unix:///var/run/cleanroom/cleanroom.sock`),
so client commands against that daemon should be run with `sudo` unless you
configure an alternate endpoint.

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


Interactive console:

```bash
cleanroom console -- bash
```

## Policy file

A `cleanroom.yaml` in your repo defines the sandbox policy. Cleanroom also checks `.buildkite/cleanroom.yaml` as a fallback.

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

Enable Docker as a guest service:

```yaml
sandbox:
  services:
    docker:
      required: true
```

Validate policy without running anything:

```bash
cleanroom policy validate
```

## Backend support

| Host OS | Backend | Status | Notes |
|---------|---------|--------|-------|
| Linux | `firecracker` | Full support | Persistent sandboxes, file download, egress allowlist enforcement |
| macOS | `darwin-vz` | Supported with gaps | Per-run VMs (no persistent sandboxes yet), no file download, no egress filtering yet |

Backend capabilities are exposed in `cleanroom doctor --json` under `capabilities`. See [isolation model](docs/isolation.md) for enforcement and persistence details.

Select a backend explicitly:

```bash
cleanroom exec --backend firecracker -- npm test
cleanroom exec --backend darwin-vz -- npm test
```

## Architecture

- **Server:** `cleanroom serve` (required for all operations)
- **Client:** CLI and ConnectRPC clients
- **Transport:** unix socket (default), [HTTPS with mTLS](docs/tls.md), or [Tailscale](docs/remote-access.md)
- **RPC services:** `cleanroom.v1.SandboxService`, `cleanroom.v1.ExecutionService` ([API design](docs/api.md))

## Go client

Use `github.com/buildkite/cleanroom/client` from external Go modules.

```go
import (
  "context"
  "os"

  "github.com/buildkite/cleanroom/client"
)

func example() error {
  c := client.Must(client.NewFromEnv())

  sb, err := c.EnsureSandbox(context.Background(), "thread:abc123", client.EnsureSandboxOptions{
    Backend: "firecracker",
    Policy: client.PolicyFromAllowlist(
      "ghcr.io/buildkite/cleanroom-base/alpine@sha256:...",
      "sha256:...",
      client.Allow("api.github.com", 443),
      client.Allow("registry.npmjs.org", 443),
    ),
  })
  if err != nil { return err }

  result, err := c.ExecAndWait(context.Background(), sb.ID, []string{"bash", "-lc", "echo hello"}, client.ExecOptions{
    Stdout: os.Stdout,
    Stderr: os.Stderr,
  })
  if err != nil { return err }
  _ = result
  return nil
}
```

## Images

Cleanroom uses digest-pinned OCI images as sandbox bases. Images are pulled from any OCI registry and materialized into ext4 rootfs files for the VM backend.

```bash
cleanroom image pull ghcr.io/buildkite/cleanroom-base/alpine@sha256:...
cleanroom image ls
cleanroom image rm sha256:...
cleanroom image import ghcr.io/buildkite/cleanroom-base/alpine@sha256:... ./rootfs.tar.gz
cleanroom image bump-ref    # resolve :latest tag to digest and update cleanroom.yaml
```

`ghcr.io/buildkite/cleanroom-base/alpine` and `ghcr.io/buildkite/cleanroom-base/alpine-docker` are published from this repo on pushes to `main`.

## Runtime config

Config path: `$XDG_CONFIG_HOME/cleanroom/config.yaml` (typically `~/.config/cleanroom/config.yaml`).

```bash
cleanroom config init
```

On macOS this defaults `default_backend` to `darwin-vz`. On Linux it defaults to `firecracker`.

```yaml
default_backend: firecracker
backends:
  firecracker:
    binary_path: firecracker
    kernel_image: ""    # auto-managed when unset
    privileged_mode: sudo
    vcpus: 2
    memory_mib: 1024
    launch_seconds: 30
  darwin-vz:
    kernel_image: ""    # auto-managed when unset
    rootfs: ""          # derived from sandbox.image.ref when unset
    vcpus: 2
    memory_mib: 1024
    launch_seconds: 30
```

When `kernel_image` is unset, Cleanroom auto-downloads a managed kernel. Set it explicitly for offline operation.

When `rootfs` is unset, Cleanroom derives one from `sandbox.image.ref` and injects the guest runtime. This requires `mkfs.ext4` and `debugfs` on the host (macOS: `brew install e2fsprogs`).

## Host requirements

**Linux ([firecracker](docs/backend/firecracker.md)):**
- `/dev/kvm` available and writable
- Firecracker binary installed
- `mkfs.ext4` for OCI-to-ext4 materialization
- `sudo -n` access for `ip`, `iptables`, `sysctl`

**macOS ([darwin-vz](docs/backend/darwin-vz.md)):**
- `cleanroom-darwin-vz` helper signed with `com.apple.security.virtualization` entitlement
- `mkfs.ext4` and `debugfs` (`brew install e2fsprogs`)

## Diagnostics

```bash
cleanroom doctor              # check host prerequisites
cleanroom doctor --json       # machine-readable with capabilities map
cleanroom status --last-run   # inspect most recent run
cleanroom status --run-id <id>
cleanroom version
```

## Further reading

- [research.md](docs/research.md) -- backend and tooling evaluation notes
- [benchmarks.md](docs/benchmarks.md) -- TTI measurement and results
- [ci.md](docs/ci.md) -- Buildkite pipeline and base image workflow
- [spec.md](docs/spec.md) -- full specification and roadmap
- [tls.md](docs/tls.md) -- certificate bootstrap, auto-discovery, HTTPS transport
- [gateway.md](docs/gateway.md) -- host-side git/registry proxy and credential injection
- [remote-access.md](docs/remote-access.md) -- Tailscale and HTTP listeners
- [isolation.md](docs/isolation.md) -- enforcement details and persistence behavior
- [api.md](docs/api.md) -- ConnectRPC surface and proto sketch
- [vsock.md](docs/vsock.md) -- guest execution protocol
- [backend/firecracker.md](docs/backend/firecracker.md) -- Firecracker backend design
- [backend/darwin-vz.md](docs/backend/darwin-vz.md) -- macOS backend and helper design
