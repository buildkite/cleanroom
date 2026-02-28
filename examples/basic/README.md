# Basic Cleanroom Example

This example is a minimal policy + command flow you can use to verify launched execution against the current client/server architecture.

## Prerequisites

From repository root:

```bash
mise run install
```

## Files

- `cleanroom.yaml`: digest-pinned sandbox image ref plus a deny-by-default network policy with one allowed host.
- `marker.sh` / `cleanup.sh`: optional host-side helpers from earlier workflows.

## Quick test flow

Run from this directory (`examples/basic`):

```bash
mise exec -- cleanroom policy validate
```

Start a local control-plane server:

```bash
mise exec -- cleanroom serve &
```

Run a command in a darwin-vz sandbox:

```bash
mise exec -- cleanroom exec --backend darwin-vz -- sh -lc 'echo basic-example-ok'
```

Expected output:

```text
basic-example-ok
```

Optional second check:

```bash
mise exec -- cleanroom exec --backend darwin-vz -- sh -lc 'cat /etc/alpine-release || cat /etc/os-release'
```

When finished:

```bash
pkill -f "cleanroom serve"
```
