# Basic Cleanroom Example

This example is a minimal policy + command set you can use to verify current MVP behavior.

## Prerequisites

From repository root:

```bash
mise run install
```

The CLI should be available in `GOBIN` (or `GOPATH/bin`).

## Files

- `cleanroom.yaml`: deny-by-default network policy with one allowed host.
- `marker.sh`: command that writes a local marker file.
- `cleanup.sh`: removes marker files created during testing.

## Quick test flow

Run from this directory (`examples/basic`):

```bash
cleanroom policy validate
```

### 1) Plan-only mode (no execution)

```bash
./cleanup.sh
cleanroom exec --dry-run -- ./marker.sh
ls -la .marker-created
```

Expected: `ls` fails because the command is not executed in plan-only mode.

### 2) Explicit host passthrough execution

```bash
cleanroom exec --host-passthrough -- ./marker.sh
ls -la .marker-created
cat .marker-created
```

Expected: marker file exists and contains a timestamp.

### 3) Cleanup

```bash
./cleanup.sh
```

## Optional: launched backend path

Launched execution requires runtime config (`~/.config/cleanroom/config.yaml`) with Firecracker `kernel_image` and `rootfs`, plus a rootfs prepared with `cleanroom-guest-agent` boot hook.

```bash
sudo ../../scripts/create-rootfs-image.sh
../../scripts/prepare-firecracker-image.sh

cleanroom exec -- ./marker.sh
ls -la .marker-created
```
