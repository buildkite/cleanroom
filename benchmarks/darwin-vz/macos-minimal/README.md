# Minimal macOS darwin-vz benchmark

This directory contains a standalone Virtualization.framework probe for macOS
guest experiments. It is not a Cleanroom backend and does not replace the
Linux `benchmarks/darwin-vz/minimal` benchmark.

The runner boots a prepared Apple Silicon macOS VM bundle, connects to a guest
agent over a VZ virtio socket, sends a small exec request, streams guest
stdout/stderr to the host, and writes timing metadata as JSON.

## Bundle layout

The runner accepts either a bundle metadata file or a directory containing
`bundle.json`. Relative paths are resolved from the metadata file's directory.

```json
{
  "schema_version": 1,
  "os": "macos",
  "arch": "arm64",
  "macos_version": "15.5",
  "macos_build": "24F74",
  "vcpus": 4,
  "memory_mib": 8192,
  "disk": "disk.img",
  "auxiliary_storage": "auxiliary.storage",
  "hardware_model": "hardware-model.bin",
  "machine_identifier": "machine-identifier.bin",
  "agent": {
    "transport": "virtio_socket",
    "port": 10700,
    "version": "0.1.0"
  },
  "display": {
    "width_px": 1024,
    "height_px": 768,
    "pixels_per_inch": 72
  }
}
```

The hardware model and machine identifier files are the opaque
Virtualization.framework data representations for `VZMacHardwareModel` and
`VZMacMachineIdentifier`. A later image-prep slice should generate and clone
these safely; this probe only validates and consumes an existing bundle.

## Build

```bash
benchmarks/darwin-vz/macos-minimal/build-runner.sh
```

The script writes `dist/darwin-vz-macos-minimal` and signs it with
`cmd/cleanroom-darwin-vz/entitlements.plist` by default.

## Validate metadata

```bash
dist/darwin-vz-macos-minimal \
  --bundle /path/to/bundle.json \
  --validate-only
```

Validation checks the manifest shape, required files, the current host's
Virtualization.framework support, and whether the hardware model can be loaded
on this host. It does not start the VM.

## Run a command

```bash
dist/darwin-vz-macos-minimal \
  --bundle /path/to/bundle.json \
  --metrics /tmp/macos-cleanroom-smoke.json \
  -- /usr/bin/sw_vers
```

The command defaults to `/usr/bin/sw_vers`. Guest stdout and stderr are
streamed to the matching host streams. The runner exits with the guest command
exit code and writes timing metadata to the path provided by `--metrics`.

The guest agent protocol is deliberately tiny for the probe:

- host sends one newline-delimited JSON request:
  `{"type":"exec","command":["/usr/bin/sw_vers"]}`
- guest sends newline-delimited JSON frames:
  `stdout`, `stderr`, and a final `exit` frame
- `stdout` and `stderr` frame `data` values are base64-encoded bytes

Slice 2 of `docs/plans/macos-cleanrooms.md` owns turning this into a packaged
macOS guest agent.
