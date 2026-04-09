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

## Install

Install the latest release:

```bash
curl -fsSL https://raw.githubusercontent.com/buildkite/cleanroom/main/scripts/install.sh | bash
```

Install a specific version:

```bash
curl -fsSL https://raw.githubusercontent.com/buildkite/cleanroom/main/scripts/install.sh | \
  bash -s -- --version vX.Y.Z
```

By default this installs to `/usr/local/bin`. Override with `--install-dir` or `CLEANROOM_INSTALL_DIR`.

On macOS, `install.sh` now prefers the signed, notarized `.pkg` when using the default install flow. It falls back to the release tarball when you request a custom install directory or helper customization.

Install the locally built binaries from this checkout into `/usr/local/bin`:

```bash
mise run install:global
```

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

Install as a daemon:

```bash
# macOS: installs a user LaunchAgent (user-scope only)
cleanroom daemon install

# Linux (systemd)
sudo cleanroom daemon install
```

Use `--force` to overwrite an existing service file. On macOS, `--system`
is unsupported; `--user` is accepted for explicitness.

Manage the daemon lifecycle:

```bash
cleanroom daemon status
cleanroom daemon start
cleanroom daemon stop
cleanroom daemon uninstall
```

The system daemon socket is root-owned (`unix:///var/run/cleanroom/cleanroom.sock`),
so client commands against that daemon should be run with `sudo` unless you
configure an alternate endpoint. User-scope daemons listen on the runtime socket
(`unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock` when `XDG_RUNTIME_DIR` is set).

Run a command in a sandbox:

```bash
cleanroom exec -- npm test
cleanroom exec -e OPENAI_API_KEY -- codex app-server
```

When `cleanroom.yaml` includes a repository bootstrap block, the top-level
commands become repo-aware: Cleanroom resolves the current git remote and local
`HEAD`, materializes that checkout in the sandbox, and starts commands in the
configured guest path. If the checked-out repository contains `.mise.toml`,
`mise.toml`, `.tool-versions`, or `.mise/config.toml`, Cleanroom also runs
the command through `mise exec -- ...` unless
`sandbox.mise.enabled: false` or `sandbox.mise.install: false` is set in
`cleanroom.yaml`.

Pre-create a long-running sandbox without running a command:

```bash
SANDBOX_ID="$(cleanroom create)"
cleanroom exec --in "$SANDBOX_ID" -- npm run lint
```

Override the sandbox image per command (remote tag/digest or local Docker image name):

```bash
cleanroom sandbox create --image ghcr.io/buildkite/cleanroom-base/alpine:latest
cleanroom exec --image ghcr.io/buildkite/cleanroom-base/alpine:latest -- npm test
cleanroom console --image my-local-image:dev -- sh
cleanroom exec -e OPENAI_API_KEY -e CODEX_HOME=/workspace/.codex -- codex app-server
```

Equivalent namespaced command:

```bash
cleanroom sandbox create
```

`cleanroom sandbox create` stays generic. It does not inspect the local git
repository or infer a checkout from `cleanroom.yaml`.

`cleanroom exec` and `cleanroom console` create ephemeral sandboxes by default.
Reuse an existing sandbox with `--in`, or keep a newly created sandbox with
`--keep`.

List sandboxes and run more commands:

```bash
cleanroom sandbox ls
cleanroom exec --in <id> -- npm run lint
cleanroom exec --in <id> -- npm run build
```

Keep a sandbox created by `exec`:

```bash
cleanroom exec --keep -- npm test
```

Run against a snapshot:

```bash
cleanroom exec --from snap_... -- npm test
cleanroom console --from snap_...
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

Repository-aware bootstrap is the default for the top-level commands when you
run them from inside a git repository.

The implicit defaults are:

```yaml
repository:
  remote: origin
  path: /workspace
  submodules: false
```

Use the optional `repository` block only to override those defaults or disable
the behavior:

```yaml
repository:
  enabled: false
```

or:

```yaml
repository:
  path: /work
  submodules: true
```

With the default behavior:

- `cleanroom create` creates a sandbox with the current repo checked out at local `HEAD`
- `cleanroom exec -- <cmd>` checks out the repo, runs `<cmd>` from `/workspace`, and tears the sandbox down unless `--keep` is set
- `cleanroom console -- bash` opens a shell in `/workspace` and tears the sandbox down unless `--keep` is set
- dirty working trees print a warning and use committed `HEAD`; uncommitted changes are not copied in
- `cleanroom sandbox create` remains explicit and repo-agnostic

Repository bootstrap needs the remote host in `sandbox.network.allow`, for
example:

```yaml
sandbox:
  network:
    default: deny
    allow:
      - host: github.com
        ports: [443]
```

## Backend support

| Host OS | Backend | Status | Notes |
|---------|---------|--------|-------|
| Linux | `firecracker` | Full support | Persistent sandboxes, per-sandbox TAP + guest IP identity, file download, egress allowlist enforcement |
| macOS | `darwin-vz` | Supported with gaps | Persistent sandboxes, `filehandle` by default with allowlist egress filtering, experimental `vmnet-shared` custom subnet host reachability, no file download, no TAP parity |

Backend capabilities are exposed in `cleanroom doctor --json` under `capabilities`. See [isolation model](docs/isolation.md) for enforcement and persistence details.

Network model differs significantly by backend:

- `firecracker` creates a dedicated TAP interface and host/guest IP pair per sandbox, which enables host-side identity and firewall enforcement.
- `darwin-vz` now defaults to `filehandle` on macOS so deny-by-default policies can use the Cleanroom-owned gateway path for allowlisted egress.
- `darwin-vz` still does not expose Firecracker-style TAP devices or host firewall enforcement semantics. Explicit `vmnet-shared` and `nat` remain available as compatibility and debugging fallbacks.
- Current vmnet work and remaining gaps are tracked in [docs/plans/darwin-vz-vmnet-mode.md](docs/plans/darwin-vz-vmnet-mode.md).

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

## Images

Cleanroom uses digest-pinned OCI images as sandbox bases. Images are pulled from any OCI registry and materialized into ext4 rootfs files for the VM backend.

```bash
cleanroom image pull ghcr.io/buildkite/cleanroom-base/alpine@sha256:...
cleanroom image ls
cleanroom image rm sha256:...
cleanroom image import ghcr.io/buildkite/cleanroom-base/alpine@sha256:... ./rootfs.tar.gz
cleanroom image bump-ref    # resolve :latest tag to digest and update cleanroom.yaml
```

`ghcr.io/buildkite/cleanroom-base/alpine`, `ghcr.io/buildkite/cleanroom-base/alpine-docker`, and `ghcr.io/buildkite/cleanroom-base/alpine-agents` are published from this repo on pushes to `main`.

Build these locally with `mise`:

```bash
mise run build:images
# or individually:
mise run build:image:alpine
mise run build:image:alpine-docker
mise run build:image:alpine-agents
```

## Runtime config

Config path: `$XDG_CONFIG_HOME/cleanroom/config.yaml` (typically `~/.config/cleanroom/config.yaml`).

```bash
cleanroom config init
```

On macOS this defaults `default_backend` to `darwin-vz`. On Linux it defaults to `firecracker`.
If `default_backend` is omitted or blank in an existing config, Cleanroom falls back to the same host default at load time.

Optional endpoint override precedence is `--host`, then `CLEANROOM_HOST`, then `control_host` from runtime config, then defaults (macOS: user runtime socket; Linux: system socket when present, otherwise user runtime socket).

```yaml
default_backend: firecracker
control_host: ""             # optional override for client endpoint resolution
backends:
  firecracker:
    binary_path: firecracker
    kernel_image: ""    # auto-managed when unset
    privileged_helper_path: /usr/local/sbin/cleanroom-root-helper
    vcpus: 2
    memory_mib: 1024
    launch_seconds: 30
  darwin-vz:
    kernel_image: ""    # auto-managed when unset
    rootfs: ""          # derived from sandbox.image.ref when unset
    network:
      mode: nat         # optional fallback; default is filehandle on macOS
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
- `sudo -n` access to `/usr/local/sbin/cleanroom-root-helper`

**macOS ([darwin-vz](docs/backend/darwin-vz.md)):**
- `cleanroom-darwin-vz` helper signed with `com.apple.security.virtualization` entitlement
- optional `vmnet-shared` additionally needs `com.apple.developer.networking.vmnet` and a matching provisioning profile
- explicit `nat` remains available as a compatibility fallback
- `mkfs.ext4` and `debugfs` (`brew install e2fsprogs`)

## Diagnostics

```bash
cleanroom doctor              # check host prerequisites
cleanroom doctor --json       # machine-readable with capabilities map
cleanroom sandbox inspect <sandbox-id>
cleanroom execution inspect --sandbox-id <sandbox-id> --last
cleanroom execution inspect --sandbox-id <sandbox-id> <execution-id>
cleanroom status --last       # browse the newest retained execution artifacts
cleanroom status --execution-id <execution-id>
cleanroom version
```

Failure flow:

- `cleanroom exec` and `cleanroom console` print `sandbox_id` and `execution_id` on failure when available.
- `cleanroom sandbox inspect <sandbox-id>` shows sandbox state plus `last_execution_id` and `active_execution_id`.
- `cleanroom execution inspect ...` is the control-plane view for execution status, retained stdout/stderr, image metadata, and observability.
- `cleanroom status ...` is the local artifact view under `$XDG_STATE_HOME/cleanroom/executions`.

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
