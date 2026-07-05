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

Check the artifact's provenance:

```bash
spore --json inspect basic.spore | cleanroom verify --dir .
```

Rebaking without changing the policy or the repo is a no-op:

```bash
cleanroom bake . --out basic.spore
```
