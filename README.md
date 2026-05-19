# 👩‍🔬 Cleanroom

<p align="left">
  <a href="https://buildkite.com/buildkite/cleanroom"><img src="https://badge.buildkite.com/0c27de9fc0d4b5e615083113c2c2503602076d7a0822c1753d.svg" alt="Build status"></a>
  <a href="https://github.com/buildkite/cleanroom/releases"><img src="https://img.shields.io/github/v/release/buildkite/cleanroom?label=Release" alt="Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-blue.svg" alt="License"></a>
</p>

Cleanroom runs CI jobs and other untrusted code in self-hosted Linux microVMs on macOS and Linux, with sub-second VM creation on the fast path and deny-by-default network policy.

It is built for CI first: fast, isolated build and test environments with the same repo, services, caches, and network rules your pipeline expects. The point is to make microVMs cheap enough for normal build and test loops, not just rare high-risk jobs. Agents are the next natural use case. They get running code they can inspect and modify, without drifting away from CI or getting broad access to the host.

The short version:

- hardware VM isolation, not containers
- sub-second VM creation on warm paths
- deny-by-default egress from `cleanroom.yaml`, including DNS resolution
- host-side gateway backed by [`content-cache`](https://github.com/buildkite/content-cache), so Git, OCI images, Go modules, RubyGems, immutable downloads, and setup outputs can be cached without giving the guest your credentials
- standard OCI images as sandbox bases
- repo-aware execution with explicit workspace copy-in and copy-out when needed
- deterministic dependency and service caches for setup declared from explicit inputs and outputs

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/buildkite/cleanroom/main/scripts/install.sh | bash
```

The installer puts `cleanroom` in `/usr/local/bin`, installs the VM helper where needed, creates runtime config, and starts the daemon.

Need a pinned version, custom install directory, or binary-only install? Use `--version`, `--install-dir`, `CLEANROOM_INSTALL_DIR`, or `--no-daemon`.

## Quick Start

Check the host, then start a repo-agnostic sandbox:

```bash
cleanroom doctor
cleanroom sandbox create --image ghcr.io/buildkite/cleanroom-base/alpine:latest
# 01kr7p9ksmfa7rmyypyqw2w8r0

cleanroom exec --in 01kr7p9ksmfa7rmyypyqw2w8r0 -- uname -a
# Linux (none) 6.1.155 #1 SMP Tue Nov 18 09:22:35 UTC 2025 aarch64 Linux

cleanroom sandbox rm 01kr7p9ksmfa7rmyypyqw2w8r0
```

That path does not inspect your local Git checkout. It is the fastest way to prove the daemon, backend, image handling, and guest execution are working.

For a repo-aware run, use a real CI repo. The [Buildkite Agent example](examples/buildkite-agent/cleanroom.yaml) keeps the policy in this repo so you can try it without changing `buildkite/agent`:

```bash
cd examples/buildkite-agent
cleanroom policy validate

cleanroom exec \
  --repo-url https://github.com/buildkite/agent.git \
  -- mise x -- go run . --version
```

That checks out the latest agent revision at `/workspace`, resolves it to an exact commit before sandbox bootstrap, warms `mise` and Go module dependencies from the policy, then builds and runs the agent CLI. In your own repo, commit and usually push `cleanroom.yaml` so the sandbox sees the same revision CI sees. Use `--copy-in` or `--sync` for local edits you have not committed or pushed yet.

## Use This For

Run risky code without giving it your host:

```bash
cleanroom sandbox create --image ghcr.io/buildkite/cleanroom-base/alpine:latest
# 01kr7p9ksmfa7rmyypyqw2w8r0

cleanroom exec --in 01kr7p9ksmfa7rmyypyqw2w8r0 -- uname -a
cleanroom console --in 01kr7p9ksmfa7rmyypyqw2w8r0 -- sh
```

Copy local work into the sandbox, or bring generated changes back:

```bash
cleanroom exec --copy-in -- mise exec -- npm test
cleanroom exec --copy-out -- mise exec -- npm run fmt
cleanroom exec --sync -- mise exec -- npm run generate
cleanroom console --sync -- sh
```

For the lower-level preview, diff, and copy commands, see [Workspaces](docs/workspaces.md).

Expose a service from the sandbox while a client process is running:

```bash
# Raw TCP: host 127.0.0.1:15432 to sandbox port 5432
cleanroom exec --expose 15432:5432 -- postgres

# Local HTTPS: usually https://buildkite.cleanroom.localhost:8143
cleanroom exec --expose-https buildkite:3000 -- mise exec -- npm run dev

# Or load HTTPS route names from expose.https in cleanroom.yaml
cleanroom exec --expose-https -- mise exec -- npm run dev
```

The installer starts the Cleanroom daemon. It does not install the macOS
resolver or HTTPS trust for `cleanroom.localhost`, and it does not install a
persistent DNS server. On macOS, run this once before using `--expose-https`
hostnames:

```bash
sudo cleanroom dns install
```

`cleanroom dns install` writes the resolver and trust material. The local DNS
listener is started by `--expose-https` and `cleanroom expose` while exposures
are active.

Configured HTTPS routes can enumerate full local hostnames or use a shared base:

```yaml
expose:
  https:
    base: "{sandbox_id}.cleanroom.localhost"
    routes:
      - port: 3000
        hosts:
          - "{base}"
          - "*.{base}"
          - "*.*.{base}"
```

`cleanroom dns install` manages `cleanroom.localhost`. Routes under another
local suffix, such as `{sandbox_id}.localhost`, also work when that suffix
resolves to the Cleanroom DNS listener.

Run Docker inside the microVM when the workload needs it:

```yaml
sandbox:
  docker:
    required: true
```

Cache expensive setup at sandbox creation time:

```yaml
sandbox:
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

Use dependency blocks for deterministic repo-local setup, service blocks for on-disk service preparation, and `sandbox.run.before` for live startup before each execution.

## Policy Basics

Cleanroom checks `cleanroom.yaml`, then `.buildkite/cleanroom.yaml`.

The policy chooses the image, minimum resources, network rules, Docker requirement, and create-time setup:

```yaml
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/debian@sha256:...
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

Use `cleanroom image resolve <image:tag>` to print a digest-pinned image ref, or `cleanroom image bump-ref <image:tag>` to update `cleanroom.yaml`.

Repository checkout is on by default for top-level commands:

```yaml
repository:
  remote: origin
  path: /workspace
  submodules: false
```

You usually do not need to write that block. Add it only when you want to override the defaults or disable repo bootstrap:

```yaml
repository:
  enabled: false
```

## How It Works

Cleanroom has a long-running server (`cleanroom serve`) and a CLI client. The default transport is a Unix socket. HTTP and HTTPS with server-auth TLS are also supported for remote control-plane access.

Each sandbox boots a Linux microVM:

| Host OS | Backend | Notes |
|---------|---------|-------|
| Linux | `firecracker` | Persistent sandboxes, per-sandbox TAP and guest IP identity, file copy, egress allowlist enforcement |
| macOS | `darwin-vz` | Persistent sandboxes, file copy, filehandle networking with allowlist egress filtering, no Firecracker-style TAP parity |

Images are standard OCI images. Cleanroom materializes them into ext4 rootfs files for the selected VM backend. You can pull, list, remove, import, resolve, and bump image refs with `cleanroom image`.

Allowed Git and package traffic goes through the host gateway. The guest can fetch what policy allows, but upstream credentials stay on the host side.

## Known Gaps

- Hostname allow rules are currently enforced from observed DNS answers plus destination IP:port. They do not distinguish co-hosted services on the same IP and port.
- `darwin-vz` does not expose Firecracker-style TAP devices or host-visible guest IP identity.
- General UDP and IPv6 allowlist policy is not at parity with the TCP gateway path.
- Dirty local files are not copied into repo-aware runs unless you use `--copy-in`, `--sync`, or the explicit workspace commands.
- macOS hosts need the signed `cleanroom-darwin-vz` helper plus `mkfs.ext4` and `debugfs` from `e2fsprogs`.

## Diagnostics

```bash
cleanroom doctor
cleanroom doctor --json
cleanroom sandbox ls
cleanroom sandbox inspect <sandbox-id>
cleanroom sandbox inspect --last
cleanroom execution ls
cleanroom execution inspect --last
cleanroom status --last
cleanroom version
```

`cleanroom exec` and `cleanroom console` keep failure stderr focused on guest output. Use `--print-sandbox-id`, `cleanroom status --last`, or `cleanroom execution inspect ...` when you need retained control-plane details.

## Development

```bash
mise run build
mise exec -- go test ./...
mise run lint-shell
```

Build base images locally:

```bash
mise run build:images
```

Install the locally built binaries into `/usr/local/bin`:

```bash
mise run install:global
```

## Docs

- [Policy](docs/policy.md) - `cleanroom.yaml`, resources, networking, Docker, dependencies, and services
- [Workspaces](docs/workspaces.md) - repo-aware execution, local edits, copy-in, copy-out, and sync
- [Networking](docs/networking.md) - deny-by-default egress, DNS, the host gateway, and service exposure
- [Caching](docs/caching.md) - content-cache routes, dependency outputs, service outputs, and storage cleanup
- [Backends](docs/backends.md) - macOS and Linux support, backend capabilities, and host requirements
- [Operations](docs/operations.md) - daemon, runtime config, diagnostics, storage, and observability
- [Snapshots](docs/snapshots.md) - create, inspect, restore, and remove sandbox filesystem snapshots
- [API](docs/api.md) - ConnectRPC control API for integrations
