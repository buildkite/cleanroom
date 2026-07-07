# 👩‍🔬 Cleanroom

<p align="left">
  <a href="https://buildkite.com/buildkite/cleanroom"><img src="https://badge.buildkite.com/0c27de9fc0d4b5e615083113c2c2503602076d7a0822c1753d.svg" alt="Build status"></a>
  <a href="https://github.com/buildkite/cleanroom/releases"><img src="https://img.shields.io/github/v/release/buildkite/cleanroom?label=Release" alt="Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-blue.svg" alt="License"></a>
</p>

Cleanroom turns a repository's declared policy into a repeatable **warm spore** —
a microVM snapshot with dependencies installed and caches hot, provenance
attached, and deny-by-default network policy enforceable wherever it runs.

Cleanroom is a thin layer over [SporeVM](https://github.com/sporevm/sporevm).
SporeVM owns the runtime: VM lifecycle, snapshots, copy-on-write fork and
fan-out, and network enforcement. Cleanroom owns the three things SporeVM has
no opinion about:

- **compile** — translate `cleanroom.yaml` into enforceable SporeVM
  configuration, failing closed on anything SporeVM cannot enforce.
- **provenance** — attach checkable facts (repo, commit, policy hash, image
  digest) to every baked artifact, and verify them later.
- **mediation** — a host-side gateway that brokers credentials into the guest
  so secrets never enter the sandbox or its captured artifact.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/buildkite/cleanroom/main/scripts/install.sh | bash
```

This installs the single `cleanroom` binary into `/usr/local/bin`. Use
`--version`, `--install-dir`, or `CLEANROOM_INSTALL_DIR` to customise.

Cleanroom drives the `spore` CLI, so install
[SporeVM](https://github.com/sporevm/sporevm) too:

```bash
mise use -g github:sporevm/sporevm@latest
```

## Workflow

Cleanroom is the build tool; spore is the run tool.

```bash
# Bake a warm spore from a repo's cleanroom.yaml (deps installed via warmup)
cleanroom bake . --out repo.spore

# Run it — starts the gateway and command cache env when provenance requires them
cleanroom run repo.spore --dir . -- /bin/sh -lc 'make test'

# Fan out copy-on-write children that share the parent's memory and disk
spore fork repo.spore --count 100 --out agents/

# Verify an artifact's provenance and see what it needs to run
spore --json inspect repo.spore | cleanroom verify
```

A minimal `cleanroom.yaml`:

```yaml
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:...
  resources:
    memory: 1gb
  network:
    default: deny
    allow:
      - host: dl-cdn.alpinelinux.org
        ports: [443]
  warmup:
    - "apk add --no-progress make"
```

## Commands

| Command | Purpose |
|---|---|
| `cleanroom bake [dir] --out <spore>` | Compile policy, boot a builder, run warmup, capture a warm spore |
| `cleanroom run <spore> --dir <repo> -- <argv...>` | Verify a warm spore, start the gateway if needed, then run it with SporeVM |
| `cleanroom content-cache serve` | Serve a persistent host content-cache for gateway-backed runs |
| `cleanroom compile [dir]` | Emit `spore create` arguments from policy (fail-closed) |
| `cleanroom stamp [dir]` | Emit provenance annotations as `spore create` arguments |
| `cleanroom verify [spore-dir]` | Verify provenance; audit the bake key against a repo with `--dir` |
| `cleanroom gateway serve` | Serve the lineage credential-mediation gateway on a Unix socket |
| `cleanroom policy validate` | Validate `cleanroom.yaml` |

`compile` and `stamp` are the composable plumbing behind `bake`; use them to
drive `spore` directly or in CI policy checks.

`cleanroom run` is the command-launch convenience layer. After it audits the
spore's bake key against the current repository policy, if that policy requests
the `content-cache` mediation service and HTTPS allow rules, it binds the
gateway and wraps the command with Git and Go environment pointing at
`http://cleanroom-gateway.spore.internal:8170/services/content-cache/...`.
`cleanroom bake` does not mutate guest config for package managers; it only
captures the warm spore and stamps policy/provenance.

For the common `content-cache`-only case, `cleanroom run` checks
`127.0.0.1:8128/health` and starts a child content-cache service for the run if
one is not already available. The backing storage is still shared on disk across
runs. To prewarm, debug, or manage the cache yourself, run:

```bash
cleanroom content-cache serve
```

By default it listens on `127.0.0.1:8128` and stores data under the user's cache
directory at `cleanroom/content-cache`. Git caching allows `github.com` by
default; use `--git-allowed-hosts` for other Git hosts. The run-scoped child
cache is started with host allowlists derived from the audited policy, and the
temporary gateway only forwards the configured cache route prefixes. For
non-cache mediation services, or custom cache upstreams, grant services through
the gateway config:

```yaml
services:
  content-cache:
    upstream: http://127.0.0.1:8128
grants:
  - match: { remote: "https://github.com/buildkite/*" }
    services: [content-cache]
```

## How it fits together

```
cleanroom bake ─▶ spore create ─▶ copy-in ─▶ warmup ─▶ spore save --out ... --stop ─▶ repo.spore
                                                │                         │
                        credentials via gateway ┘        provenance annotations
```

- **Enforcement lives in the spore manifest.** Network rules are applied by
  SporeVM on every resume and fork, regardless of who invokes it. `cleanroom
  verify` is integrity and UX, not the security boundary.
- **Provenance rides in SporeVM annotations** and is merged into every
  snapshot, so a `.spore` handed to you is traceable to the repo and policy
  that produced it.
- **The gateway is scoped to a spore lineage** — a baked spore and all its
  forks share one gateway; authorization is by the bound socket, attribution
  by guest-presented identity. Secrets stay host-side.

## Documentation

- [Policy reference](docs/policy.md)
- [SporeVM layer plan](docs/plans/sporevm-layer.md) — the design and its slices
- [CI setup](docs/ci.md)
- [Basic example](examples/basic/README.md)
