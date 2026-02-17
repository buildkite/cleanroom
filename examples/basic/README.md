# Basic Cleanroom Example

This example is a minimal policy + command set you can use to verify current MVP behavior.

## Prerequisites

From repository root:

```bash
mise run build:cleanroom
```

The CLI binary should exist at `dist/cleanroom`.

## Files

- `cleanroom.yaml`: deny-by-default network policy with one allowed host.
- `marker.sh`: command that writes a local marker file.
- `cleanup.sh`: removes marker files created during testing.

## Quick test flow

Run from this directory (`examples/basic`):

```bash
../../dist/cleanroom policy validate
```

### 1) Plan-only default (no execution)

```bash
./cleanup.sh
../../dist/cleanroom exec -- ./marker.sh
ls -la .marker-created
```

Expected: `ls` fails because the command is not executed in plan-only mode.

### 2) Explicit host passthrough execution

```bash
../../dist/cleanroom exec --host-passthrough -- ./marker.sh
ls -la .marker-created
cat .marker-created
```

Expected: marker file exists and contains a timestamp.

### 3) Cleanup

```bash
./cleanup.sh
```

## Optional: launched VM path

Launched VM execution requires Firecracker plus a kernel/rootfs that starts `cleanroom-guest-agent`:

```bash
../../dist/cleanroom exec \
  --launch \
  --kernel-image /path/to/vmlinux \
  --rootfs /path/to/rootfs.ext4 \
  -- /bin/echo hello
```
