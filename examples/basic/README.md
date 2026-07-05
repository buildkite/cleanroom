# Basic Cleanroom Example

A minimal policy you can bake into a warm spore and run.

## Prerequisites

From the repository root:

```bash
mise run install          # installs the cleanroom binary
mise use -g github:sporevm/sporevm@latest   # installs spore
```

## Files

- `cleanroom.yaml`: digest-pinned image ref plus a deny-by-default network
  policy with one allowed host.

## Flow

Run from this directory (`examples/basic`):

```bash
cleanroom policy validate
cleanroom bake . --out basic.spore
spore run --from basic.spore 'echo basic-example-ok'
```

Expected output:

```text
basic-example-ok
```

Check the artifact's provenance and audit it against the repo:

```bash
cleanroom verify basic.spore --dir .
```

(The positional spore path lets the audit exclude the in-repo artifact from
git dirty detection; the stdin form `spore --json inspect ... | cleanroom
verify` has no path to exclude, so pair it with an artifact stored outside
the repository.)

Rebaking without changing the policy or the repo is a no-op:

```bash
cleanroom bake . --out basic.spore
```
