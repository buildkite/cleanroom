# Snapshots

Snapshots save a sandbox filesystem so future sandboxes can fresh-boot from
that saved state.

They are not live process or memory checkpoints. A restored sandbox gets a new
VM, network identity, gateway registration, and execution lifecycle.

## Create And Inspect

```bash
cleanroom sandbox create --image ghcr.io/buildkite/cleanroom-base/alpine:latest
# 01kr7p9ksmfa7rmyypyqw2w8r0

cleanroom exec --in 01kr7p9ksmfa7rmyypyqw2w8r0 -- sh -lc 'apk add --no-cache git'

cleanroom snapshot create --name git-tools 01kr7p9ksmfa7rmyypyqw2w8r0
# snap_01kr7r6y7xqqpmeb0n6sk77s14

cleanroom snapshot inspect snap_01kr7r6y7xqqpmeb0n6sk77s14
cleanroom snapshot ls
```

Use `--json` with `snapshot create`, `snapshot inspect`, or `snapshot ls` when
you need machine-readable output.

## Restore

Create a new sandbox from a snapshot:

```bash
cleanroom sandbox create --from snap_01kr7r6y7xqqpmeb0n6sk77s14
```

Run a command in a fresh sandbox from a snapshot:

```bash
cleanroom exec --from snap_01kr7r6y7xqqpmeb0n6sk77s14 -- git --version
```

Start an interactive shell from a snapshot:

```bash
cleanroom console --from snap_01kr7r6y7xqqpmeb0n6sk77s14 -- sh
```

## Delete

```bash
cleanroom snapshot rm snap_01kr7r6y7xqqpmeb0n6sk77s14
```

`cleanroom system prune` does not delete explicit snapshots by default.

## Backend Support

Snapshot support depends on backend runtime config and host support:

- `darwin-vz` uses the `apfs` snapshot driver when available.
- `firecracker` supports file-backed snapshots and can use `zfs` when configured
  on a capable host.

Run `cleanroom doctor` when snapshot commands report that the runtime is
disabled or unsupported.
