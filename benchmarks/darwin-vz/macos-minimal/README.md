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

For setup or manual image finalization, build the viewer:

```bash
benchmarks/darwin-vz/macos-minimal/build-viewer.sh
```

The viewer writes `dist/darwin-vz-macos-viewer`. It boots the same bundle in a
`VZVirtualMachineView` window and can expose one host directory read-only using
the macOS guest automount tag. Pass `--validate-only` to check the bundle and
share configuration without starting the VM. For headless diagnostics, pass
`--screenshot /tmp/vm.png`; the viewer writes the PNG from inside its own
process after the VM starts, which works even when host `screencapture` cannot
capture the VZ window.

## Create a local bundle from an IPSW

```bash
benchmarks/darwin-vz/macos-minimal/build-create-bundle.sh

dist/darwin-vz-macos-create-bundle \
  --ipsw /path/to/UniversalMac.ipsw \
  --out /path/to/cleanroom-macos-bundle \
  --disk-size-gib 120
```

The create-bundle tool installs macOS from a local Apple Silicon IPSW into the
same bundle layout consumed by the runner:

- `bundle.json`
- `disk.img`
- `auxiliary.storage`
- `hardware-model.bin`
- `machine-identifier.bin`

It chooses CPU and memory defaults that satisfy the restore image requirements
and writes the macOS version/build discovered from the IPSW. The default
`agent.version` is `uninstalled`; use `--vcpus`, `--memory-mib`, `--agent-port`,
`--agent-version`, and `--display` to override the metadata written to
`bundle.json`.

This only prepares the VM bundle. It does not install the Cleanroom macOS guest
agent inside the guest. Before the runner can execute commands, boot the bundle
once, finish macOS setup, install the guest agent as a LaunchDaemon, then shut
the guest down cleanly.

## Prepare an agent bundle

Build the minimal macOS guest agent:

```bash
benchmarks/darwin-vz/macos-minimal/build-guest-agent.sh
```

Build the package that can install and bootstrap the LaunchDaemon inside a
running guest:

```bash
benchmarks/darwin-vz/macos-minimal/build-guest-agent-pkg.sh
```

Then clone a base bundle and install the agent into the clone's APFS Data
volume:

```bash
benchmarks/darwin-vz/macos-minimal/prepare-agent-bundle.sh \
  --base /path/to/cleanroom-macos-base \
  --out /path/to/cleanroom-macos-agent \
  --force
```

The prepare script leaves the base bundle untouched. It installs
`/usr/local/bin/cleanroom-macos-guest-agent`, writes
`/Library/LaunchDaemons/com.buildkite.cleanroom.macos-guest-agent.plist`, and
updates `bundle.json` with the installed agent version.

When the script is run without root privileges, it may be unable to set
root-owned metadata on the installed files. By default it fails in that case
because launchd may reject the daemon. Use `--allow-unverified-ownership` only
for inspecting the offline image flow; a bundle prepared that way is not
command-runnable until the live smoke proves the agent starts.

For a setup boot or manual guest finalization, copy
`dist/cleanroom-macos-guest-agent.pkg` into the guest and run:

```bash
sudo installer -pkg /path/to/cleanroom-macos-guest-agent.pkg -target /
```

One local way to make the package available to the guest is to boot the bundle
with the viewer and share `dist/`:

```bash
dist/darwin-vz-macos-viewer \
  --bundle /path/to/cleanroom-macos-agent \
  --shared-directory dist
```

Inside the guest, the package is available at:

```bash
/Volumes/My Shared Files/cleanroom-macos-guest-agent.pkg
```

If the guest stops at loginwindow, the image still needs user/session
finalization before an agent installed as a LaunchAgent can run. The root-owned
LaunchDaemon path below avoids requiring a logged-in user, but it must be
installed by a privileged in-guest step or by a privileged offline image-prep
step.

The package is script-only so it does not archive host-side AppleDouble
metadata. Its postinstall runs inside the guest as root, writes the agent and
LaunchDaemon plist as `root:wheel`, then attempts to load and start
`com.buildkite.cleanroom.macos-guest-agent`.

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
  `{"command":["/usr/bin/sw_vers"]}`
- host sends `{"type":"eof"}` immediately after the request because this
  runner does not forward interactive stdin yet
- guest sends newline-delimited JSON frames:
  `stdout`, `stderr`, and a final `exit` frame
- `stdout` and `stderr` frame `data` values are base64-encoded bytes

Slice 2 of `docs/plans/macos-cleanrooms.md` owns turning this into a packaged
macOS guest agent.
