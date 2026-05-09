# 👩‍🔬 Cleanroom

Cleanroom runs untrusted code in microVMs with deny-by-default network policy. It is self-hosted, enforces repository-scoped egress rules, and keeps credentials on the host side of the VM boundary.

Agent sandboxing tools are [proliferating fast](docs/research.md). Most focus on isolation alone. Cleanroom adds policy-controlled network access so you decide exactly what the sandbox can reach.

## Why Cleanroom?

**Deny-by-default egress.** A `cleanroom.yaml` policy file in your repo controls which hosts the sandbox may reach. Current hostname-based rules are enforced from observed DNS answers plus destination IP:port, so co-hosted services on the same IP:port are not distinguished. Everything else is blocked.

**MicroVM isolation.** Each sandbox is a hardware-virtualized microVM (Firecracker on Linux, Virtualization.framework on macOS), not a container. A VM boundary is stronger than namespaces, seccomp, or gVisor -- a kernel vulnerability in the guest doesn't compromise the host.

**Self-hosted.** Runs on your infrastructure. Your code and data never leave your machines.

**Credentials stay on the host.** A [host-side gateway](docs/gateway.md) rewrites git traffic through Cleanroom-owned routes and keeps upstream credentials on the host side of the boundary. The same gateway now embeds `content-cache` for cache-backed git, OCI, Go module, RubyGems, and immutable download handling.

**Standard OCI images.** Use any OCI image from any registry as your sandbox base. Digest-pinned in policy for reproducibility. No custom VM image format or vendor-specific base images. Same image works across backends.

**Docker inside the sandbox.** Enable a guest Docker daemon with a single policy flag (`docker.required: true`). Docker Hub pulls are mirrored through the host gateway cache, and you can build and run containers inside the microVM.

**Deterministic setup caches.** Declare dependency and service inputs and outputs in policy. Cleanroom runs setup commands during sandbox creation, captures the paths tools naturally write, and restores matching outputs on later sandboxes.

**Coming soon:** broader guest-side package-manager rewrites with lockfile enforcement, broader non-Docker-Hub registry caching, hermetic offline build flows, and richer audit surfaces. See the [spec](docs/spec.md) for the full roadmap.

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
cleanroom config validate
cleanroom doctor
```

Start the server (all CLI commands need a running server):

```bash
cleanroom serve &
```

The server listens on `unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock` by default.
When observability is enabled, `cleanroom serve` also prints startup status for
trace export, sampling, and whether direct trace links are configured.

For observability setup, local Grafana/Tempo/Prometheus development, runtime
config examples, and trace diagnostics, see [docs/observability.md](docs/observability.md).

Install as a daemon:

```bash
# macOS: installs a user LaunchAgent
cleanroom daemon install --restart

# Linux (systemd)
sudo cleanroom daemon install --restart
```

Use `cleanroom daemon install --init-config --restart` for first-run bootstrap
when the runtime config file does not exist yet.
Use `cleanroom daemon restart --force` to start the daemon again if it is
currently stopped. `--system` is unsupported on macOS; `--user` is accepted for
explicitness.

Manage the daemon lifecycle:

```bash
cleanroom daemon status
cleanroom daemon start
cleanroom daemon stop
cleanroom daemon restart
cleanroom daemon uninstall
```

The system daemon socket is root-owned (`unix:///var/run/cleanroom/cleanroom.sock`),
so client commands against that daemon should be run with `sudo` unless you
configure an alternate endpoint. User-scope daemons listen on the runtime socket
(`unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock` when `XDG_RUNTIME_DIR` is set).

Install host DNS and local HTTPS trust for named exposures on macOS:

```bash
sudo cleanroom dns install
```

This installs `/etc/resolver/cleanroom.localhost`, creates a managed local
exposure certificate under the invoking user's Cleanroom TLS directory, and
trusts that certificate for SSL in the invoking user's login keychain.
`cleanroom dns status` reports both resolver and certificate trust state.
`cleanroom dns uninstall` removes the managed resolver and certificate
material. The foreground `exec`, `expose`, or `port-forward` process owns, or
takes over, the local DNS server while exposures are live. HTTPS exposures use
`127.0.0.1:8143` when available; parallel exposure processes fall back to another
loopback port and print that port in their URLs.

Run a command in a sandbox:

```bash
cleanroom exec -- npm test
cleanroom exec --tty -e OPENAI_API_KEY -- codex app-server
```

When `cleanroom.yaml` includes a repository bootstrap block, the top-level
commands become repo-aware: Cleanroom resolves the current git remote and local
`HEAD`, materializes that checkout in the sandbox, and starts commands in the
configured guest path. Cleanroom no longer auto-detects or auto-wraps commands
for `mise`; if you want `mise`, run it explicitly in the command you execute or
in a dependency or service block command so it can participate in create-time
stage caching.

You can also define create-time and per-execution setup:

```yaml
sandbox:
  docker:
    required: true
  dependencies:
    - name: node
      command: npm ci
      inputs:
        files: [package.json, package-lock.json]
      outputs:
        dirs: [node_modules]
  services:
    - name: database
      command: |
        docker compose up -d postgres valkey
        bin/rails db:prepare
        docker compose stop postgres valkey
      inputs:
        files: [docker-compose.yml, db/schema.rb]
      outputs:
        dirs: [/var/lib/docker]
  run:
    before: docker compose up -d postgres valkey
```

Use `sandbox.dependencies` blocks for deterministic repo-local bootstrap,
`sandbox.services` blocks for snapshotable on-disk service preparation, and
`sandbox.run.before` for live startup that must happen before each execution.
Set `sandbox.docker.required: true` when the sandbox needs the guest Docker
daemon.

Dependency and service outputs are guest paths. Use the path the tool normally
writes: `node_modules` for npm, `vendor/bundle` for Bundler, `/var/lib/docker`
for Docker daemon state, or another real data directory from your service.
Relative outputs resolve against `repository.path`, so `node_modules` defaults
to `/workspace/node_modules`. Existing output directories in the repository,
such as `public/assets/.keep`, are copied into the output store on cache misses
before the setup command runs.

Dependency and service block commands support either a shell string or an argv
sequence. Prefer the string form unless you specifically need exact argv
semantics.
`sandbox.run.before` always runs through `sh -lc`.

Pre-create a long-running sandbox without running a command:

```bash
SANDBOX_ID="$(cleanroom create)"
cleanroom exec --in "$SANDBOX_ID" -- npm run lint
```

Override the sandbox image per command (remote tag/digest or local Docker image name):

```bash
cleanroom sandbox create --image ghcr.io/buildkite/cleanroom-base/debian:latest
cleanroom exec --image ghcr.io/buildkite/cleanroom-base/debian:latest -- npm test
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

Expose sandbox ports for local development:

```bash
# Raw TCP: host 127.0.0.1:15432 -> sandbox port 5432
cleanroom exec --expose 15432:5432 -- postgres

# HTTPS route, usually https://buildkite.cleanroom.localhost:8143
cleanroom exec --expose-https buildkite:3000 -- npm run dev

# Keep forwarding from this client to an existing sandbox
cleanroom expose --in <id> --expose-https buildkite:3000
cleanroom port-forward --in <id> 15432:5432
```

`--expose <port>` maps the same host and guest port. `--expose-https <port>`
uses the sandbox ID as the hostname label, and `--expose-https <name>:<port>`
uses the provided DNS label under `cleanroom.localhost`. Exposure requests are
owned by the client process that starts them; they are not stored in
`cleanroom.yaml` or the sandbox record.

Copy a one-off file into or out of a kept sandbox:

```bash
cleanroom cp ./fixture.json <id>:/tmp/fixture.json
cleanroom cp <id>:/tmp/result.json ./result.json
```

Copy workspace changes through the Git-backed workspace sync flow:

```bash
# Copy local tracked and unignored workspace changes into an existing sandbox
cleanroom workspace copy-in --dry-run <id>
cleanroom workspace copy-in <id>

# Inspect sandbox changes before writing anything locally
cleanroom workspace diff <id>
cleanroom workspace copy-out --dry-run <id>

# Copy sandbox workspace changes back into the matching local checkout
cleanroom workspace copy-out <id>
cleanroom workspace copy-out --force <id>
```

Top-level commands can run the same operations automatically:

```bash
cleanroom exec --copy-in -- npm test
cleanroom exec --copy-out -- npm run fmt
cleanroom exec --sync -- npm run generate
cleanroom console --sync -- sh
```

Workspace copy-in/copy-out currently requires a matching local Git worktree.
Ignored paths such as dependency directories and build caches are excluded by
default. Copy-out refuses local conflicts unless `--force` is used, and
non-Git directories should use explicit `cleanroom cp` paths for now.

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
cleanroom exec --tty -- bash
```

Temporarily disable egress filtering for a newly created repository sandbox:

```bash
cleanroom console --dangerously-allow-all -- bash
cleanroom exec --dangerously-allow-all -- npm test
```

## Policy file

A `cleanroom.yaml` in your repo defines the sandbox policy. Cleanroom also checks `.buildkite/cleanroom.yaml` as a fallback.

```yaml
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/debian@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  resources:
    vcpus: 4
    memory: 8GiB
    disk: 16GiB
  network:
    default: deny
    allow:
      - api.github.com:443
      - registry.npmjs.org:443
```

`sandbox.resources` is optional and declares backend-neutral minimum workload
requirements. `vcpus` is a positive integer, while `memory` and `disk` accept
raw bytes or human-friendly sizes such as `4096MiB`, `8GiB`, or `16GiB`.
Cleanroom raises the selected backend runtime config to meet these minimums,
but does not lower larger host defaults.

Enable Docker as a guest service:

```yaml
sandbox:
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
- `cleanroom exec --tty -- <cmd>` runs `<cmd>` from `/workspace` with a real tty and tears the sandbox down unless `--keep` is set
- `cleanroom console -- bash` opens a shell in `/workspace` using the same interactive tty transport and tears the sandbox down unless `--keep` is set
- dirty working trees print a warning and use committed `HEAD`; uncommitted changes are not copied in
- `cleanroom sandbox create` remains explicit and repo-agnostic

Repository bootstrap needs the remote host in `sandbox.network.allow`, for
example:

```yaml
sandbox:
  network:
    default: deny
    allow: github.com:443
```

## Backend support

| Host OS | Backend | Status | Notes |
|---------|---------|--------|-------|
| Linux | `firecracker` | Full support | Persistent sandboxes, per-sandbox TAP + guest IP identity, file copy, egress allowlist enforcement |
| macOS | `darwin-vz` | Supported with gaps | Persistent sandboxes, file copy, `filehandle` networking with allowlist egress filtering, no TAP parity |

Backend capabilities are exposed in `cleanroom doctor --json` under `capabilities`. See [isolation model](docs/isolation.md) for enforcement and persistence details.
File copy uses streaming file primitives, and the API also exposes path and archive primitives for larger sync and diff workflows.

Network model differs significantly by backend:

- `firecracker` creates a dedicated TAP interface and host/guest IP pair per sandbox, which enables host-side identity and firewall enforcement.
- `darwin-vz` uses `filehandle` networking on macOS so deny-by-default policies can use the Cleanroom-owned gateway path for allowlisted egress.
- `darwin-vz` still does not expose Firecracker-style TAP devices or host firewall enforcement semantics.

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
      "ghcr.io/buildkite/cleanroom-base/debian@sha256:...",
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

`client.ExecAndWait` is the batch-oriented helper. Interactive attach flows use
the lower-level execution RPCs (`CreateExecution`, `AttachExecution`, and
related methods).

## Images

Cleanroom uses digest-pinned OCI images as sandbox bases. Images are pulled from any OCI registry and materialized into ext4 rootfs files for the VM backend.

```bash
cleanroom image pull ghcr.io/buildkite/cleanroom-base/debian@sha256:...
cleanroom image ls
cleanroom image rm sha256:...
cleanroom image import ghcr.io/buildkite/cleanroom-base/debian@sha256:... ./rootfs.tar.gz
cleanroom image bump-ref ghcr.io/buildkite/cleanroom-base/debian:latest
                           # resolve :latest tag to digest and update cleanroom.yaml
```

Recommended defaults are the Debian-based images: `ghcr.io/buildkite/cleanroom-base/debian`, `ghcr.io/buildkite/cleanroom-base/debian-ruby`, `ghcr.io/buildkite/cleanroom-base/debian-docker`, and `ghcr.io/buildkite/cleanroom-base/debian-agents`.
The Alpine variants remain available as smaller musl-based alternatives: `ghcr.io/buildkite/cleanroom-base/alpine`, `ghcr.io/buildkite/cleanroom-base/alpine-docker`, and `ghcr.io/buildkite/cleanroom-base/alpine-agents`.

Build these locally with `mise`:

```bash
mise run build:images
# or individually:
mise run build:image:debian
mise run build:image:debian-ruby
mise run build:image:debian-docker
mise run build:image:debian-agents
mise run build:image:alpine
mise run build:image:alpine-docker
mise run build:image:alpine-agents
```

## Runtime config

Config path: `$XDG_CONFIG_HOME/cleanroom/config.yaml` (typically `~/.config/cleanroom/config.yaml`).

```bash
cleanroom config init
cleanroom config validate
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
      mode: filehandle  # optional; this is the only supported darwin-vz mode
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
- `mkfs.ext4` and `debugfs` (`brew install e2fsprogs`)

## Diagnostics

```bash
cleanroom doctor              # check host prerequisites
cleanroom doctor --json       # machine-readable with capabilities map
cleanroom sandbox inspect <sandbox-id>
cleanroom sandbox inspect --last
cleanroom execution ls        # list active executions
cleanroom execution inspect --last
cleanroom execution inspect --sandbox-id <sandbox-id> --last
cleanroom execution inspect <execution-id>
cleanroom status --last       # browse the newest retained execution artifacts
cleanroom status --execution-id <execution-id>
cleanroom version
```

Failure flow:

- `cleanroom exec` and `cleanroom console` keep failure stderr focused on streamed guest output; they do not append `sandbox_id`, `execution_id`, `trace_id`, or `trace_url` footers automatically.
- Use `--print-sandbox-id` when you need to correlate a kept or reused sandbox, and use `cleanroom status --last` or `cleanroom execution inspect ...` for retained diagnostics.
- Attached `cleanroom exec` and `cleanroom console` streams may print warning notices on stderr for policy observations such as blocked connections or disallowed DNS lookups.
- `cleanroom sandbox inspect <sandbox-id>` and `cleanroom sandbox inspect --last` show sandbox state plus `last_execution_id` and `active_execution_id`.
- `cleanroom execution ls` lists active executions by default; add `--all` to include finished executions that are still known to the control plane.
- `cleanroom execution inspect ...` is the control-plane view for execution status, retained stdout/stderr, image metadata, `trace_id`, optional `trace_url`, and observability.
- `cleanroom status ...` is the local artifact view under `$XDG_STATE_HOME/cleanroom/executions`.

## Further reading

Terraform provisioning and private host bootstrap automation now live in the
private sibling repo `../cleanroom-ops`.

- [research.md](docs/research.md) -- backend and tooling evaluation notes
- [benchmarks.md](docs/benchmarks.md) -- TTI measurement and results
- [ci.md](docs/ci.md) -- Buildkite pipeline and base image workflow
- [spec.md](docs/spec.md) -- full specification and roadmap
- [tls.md](docs/tls.md) -- certificate bootstrap, auto-discovery, HTTPS transport
- [gateway.md](docs/gateway.md) -- host-side git/registry proxy and credential injection
- [remote-access.md](docs/remote-access.md) -- Tailscale and HTTP listeners
- [isolation.md](docs/isolation.md) -- enforcement details and persistence behavior
- [api.md](docs/api.md) -- ConnectRPC surface and proto sketch
- [observability.md](docs/observability.md) -- OTLP config, local stack, and trace diagnostics
- [vsock.md](docs/vsock.md) -- guest execution protocol
- [backend/firecracker.md](docs/backend/firecracker.md) -- Firecracker backend design
- [backend/darwin-vz.md](docs/backend/darwin-vz.md) -- macOS backend and helper design
