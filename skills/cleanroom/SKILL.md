---
name: cleanroom
description: Use when an agent should bake a repository into a warm SporeVM snapshot with cleanroom, validate or author cleanroom.yaml policy, verify a spore's provenance, or serve the credential-mediation gateway for baked spores.
---

# Cleanroom

Cleanroom is a thin layer over [SporeVM](https://github.com/sporevm/sporevm):
it compiles repository policy into enforceable spore configuration, bakes a
warm `.spore` snapshot with dependencies installed, attaches checkable
provenance, and mediates credentials into running spores via a host-side
gateway. Cleanroom is the build tool; `spore` is the run tool. This skill
assumes both `cleanroom` and `spore` are installed.

## Start Here

1. Confirm the CLI is available with `cleanroom version` when install state
   matters. If flag availability is unclear, run `cleanroom <command> --help`
   before guessing.
2. Inspect policy before expensive work. Cleanroom reads `cleanroom.yaml`,
   then `.buildkite/cleanroom.yaml`:

```bash
cleanroom policy validate
cleanroom policy validate --chdir <path>   # validate another directory
cleanroom policy validate --json           # machine-readable
```

## Bake And Run

Bake a repository into a warm spore, then run work from it with spore:

```bash
cleanroom bake . --out repo.spore
spore run --from repo.spore 'make test'
```

- Dependencies install during bake via `sandbox.warmup` policy commands, so
  restores are fast and repeatable.
- Rebaking with an unchanged policy and commit is a no-op; a dirty worktree
  always rebakes and records `workspace.git.dirty=true`.
- Fan out copy-on-write children that share the parent's memory and disk:

```bash
spore fork repo.spore --count 10 --out agents/
```

## Verify Provenance

Check what produced an artifact and what it needs to run:

```bash
cleanroom verify repo.spore                  # provenance facts and run hint
cleanroom verify repo.spore --dir .          # audit bake key against the repo
spore --json inspect repo.spore | cleanroom verify   # from inspect output
```

Verify fails closed: foreign or forged manifests are rejected. `--dir` also
proves the artifact matches the repository's current policy and commit.

## Mediated Credentials

When policy requests `sandbox.mediation.services`, the spore reaches
credentials only through a host-side gateway; secrets never enter the guest
or its captured artifact:

```bash
cleanroom gateway serve --dir . --for repo.spore --socket gw.sock &
spore run --from repo.spore --bind-service cleanroom-gateway:8170=unix:gw.sock 'COMMAND'
```

`--dir` is the trust root: grants resolve from the repository's own policy
and git facts, and `--for` audits the spore's bake key against it before
serving. Operator grants live in `~/.config/cleanroom/gateway.yaml`.

## Composable Plumbing

`compile` and `stamp` are the pieces behind `bake`; use them to drive spore
directly or in CI policy checks:

```bash
eval "spore create name $(cleanroom compile .) $(cleanroom stamp .)"
```

## Policy Quick Reference

```yaml
version: 1
sandbox:
  image:
    ref: ghcr.io/org/image@sha256:...   # digest-pinned, required
  resources:
    memory: 1gb
  network:
    default: deny                        # required
    allow:
      - host: github.com
        ports: [443]
  warmup:
    - "npm ci"
  mediation:
    services: [github-token]
```

Compile fails closed on anything SporeVM cannot enforce (stage-scoped
network, docker, dependency/service blocks, `run.before`, IPv6 literal
hosts). See `docs/policy.md` for the full reference.
