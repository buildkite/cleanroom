# macOS Cleanrooms Tart Replacement Plan

**Status:** Proposed
**Last reviewed:** 2026-06-01
**Spec references:** `docs/backends.md`, `docs/backend/darwin-vz.md`, `benchmarks/darwin-vz/minimal/README.md`, `docs/research.md`

## Summary

Cleanroom can plausibly replace Tart for macOS CI workloads, but not by
turning the current `darwin-vz` backend from Linux into macOS. The current
macOS host backend is a Linux guest backend: Go resolves an OCI-derived ext4
rootfs and Linux kernel, the Swift helper starts that guest with
`VZLinuxBootLoader`, and the guest-side command protocol is implemented by the
Linux `cleanroom-guest-agent`.

The replacement path is a parallel macOS guest platform under the same
Virtualization.framework helper and control-plane shape. Apple provides the
required primitives for macOS guests on Apple Silicon, and Tart's source and
docs prove the operational model is practical: install from IPSW, persist a
disk plus auxiliary storage, run with `VZMacOSBootLoader`, expose host
directories through VirtioFS, and execute commands through a guest agent over a
virtio socket.

The first useful target should be narrow: run a Buildkite-style command in an
ephemeral macOS VM cloned from a prepared image, without invoking `tart`. Full
Cleanroom parity requires more work: macOS VM image lifecycle, a macOS guest
agent, file transfer/workspace setup, policy-grade networking, OCI
distribution or Tart-image import, and real host validation for Xcode/keychain
workloads.

## Problem

Tart currently solves three operational problems for macOS CI: distributing
macOS VM images, cloning and starting those images quickly, and running a job
command in the guest. It does not solve Cleanroom's stronger contract around
repository-scoped policy, host-held credentials, auditability, and fail-closed
capability reporting.

Cleanroom already has a macOS host backend, but that backend only runs Linux
guests. If we present that backend as "macOS Cleanrooms" without a separate
macOS guest path, we will either preserve Tart as the real execution layer or
ship a VM runner that bypasses the security and policy behavior that makes
Cleanroom worth using.

## Goals

- Replace Tart at runtime for a first Buildkite macOS CI workflow: clone,
  start, exec, stream logs, collect exit code, and tear down.
- Keep the public contract backend-neutral. Users ask for a macOS guest
  platform and an image, not a new backend name such as `darwin-vz-macos`.
- Reuse the existing `darwin-vz` helper/control split where it helps, but keep
  the Linux guest path unchanged until the macOS path proves itself.
- Support command execution without SSH by installing a Cleanroom macOS guest
  agent into the image.
- Preserve Cleanroom's policy story before declaring replacement parity:
  deny-by-default networking, stage-scoped policy swaps, gateway mediation, and
  machine-readable capability gaps.
- Make image identity explicit enough for CI: macOS build, hardware model,
  disk/auxiliary storage digests, guest-agent version, and clone semantics.

## Non-Goals

- Do not implement every Tart command or flag.
- Do not build Orchard-style cluster orchestration.
- Do not make GUI automation, VNC, audio, clipboard, or Screen Sharing part of
  the first supported surface.
- Do not support Intel macOS guests or cross-architecture emulation.
- Do not make VZ NAT plus SSH the definition of a Cleanroom sandbox.
- Do not make Tart's OCI media types the native Cleanroom image contract,
  although an importer is useful for migration.

## Evidence

### Current Cleanroom State

`docs/backends.md` describes Cleanroom as running Linux microVMs on macOS and
Linux. The macOS backend is `darwin-vz`, but the guest is still Linux.

`docs/backend/darwin-vz.md` says the helper request takes `kernel_path` and
`rootfs_path`, and the implemented flow resolves a Linux kernel, derives an
ext4 rootfs, injects the Linux guest runtime, and starts the helper-managed VM.

`cmd/cleanroom-darwin-vz/main.swift` confirms this mechanically:

- `StartVM` requires an absolute Linux kernel path and rootfs path.
- `buildVM` constructs `VZLinuxBootLoader(kernelURL:)`.
- boot args pass `root=/dev/vda`, `init=/sbin/cleanroom-init`, and the
  Cleanroom guest port.
- networking relies on a static guest IP carried through Linux boot args.

That code is a useful foundation because the helper process, lifecycle calls,
filehandle networking, and socket proxy model already exist. It is not a
macOS guest implementation.

### Apple Virtualization.framework

The macOS 26.5 SDK on this host includes the macOS guest APIs Cleanroom would
need:

- `VZMacOSBootLoader`
- `VZMacPlatformConfiguration`
- `VZMacOSRestoreImage`
- `VZMacOSInstaller`
- `VZMacAuxiliaryStorage`
- `VZMacOSVirtualMachineStartOptions`
- `VZVirtioFileSystemDeviceConfiguration`
- `VZVirtioSocketDeviceConfiguration`

The local host is Apple Silicon on macOS 26.3, and
`VZVirtualMachine.isSupported` returned `true`. Fetching Apple's latest restore
image catalog failed locally with `VZErrorRestoreImageCatalogLoadFailed`, so
the prototype should not depend on that catalog path as its only image source.
It should accept a local IPSW or a prebuilt VM bundle.

Apple's public docs describe the same model: macOS guests use
`VZMacPlatformConfiguration`, `VZMacOSBootLoader`, CPU/memory requirements from
`VZMacOSConfigurationRequirements`, a main storage device, and input/graphics
devices. Installing from a restore image is done with `VZMacOSInstaller`.
VirtioFS can automount shared host directories inside macOS guests under
`/Volumes/My Shared Files`.

### Tart Replacement Surface

Tart is the right comparison point because it already packages this stack for
CI:

- it publishes macOS 12 through macOS 26 VM images, including base and Xcode
  variants
- its Buildkite integration runs a pipeline command inside a cloned macOS VM
- it can create macOS VMs from IPSW
- it stores and pulls/pushes VM images through OCI-compatible registries
- it has a guest agent for `tart exec`, clipboard, and disk resize
- it uses VirtioFS directory sharing for host workspace mounts

I checked Tart source at commit
`5287b597a14773eb9b627ccd0545c675ef8a59f5`. The relevant implementation shape:

- `Sources/tart/Platform/Darwin.swift` maps macOS guests to
  `VZMacOSBootLoader`, `VZMacPlatformConfiguration`,
  `VZMacAuxiliaryStorage`, `VZMacGraphicsDeviceConfiguration`, and macOS input
  devices.
- `Sources/tart/VM.swift` installs from IPSW with `VZMacOSRestoreImage` and
  `VZMacOSInstaller`, then builds a VM configuration with disk, network,
  directory sharing, console devices, and a virtio socket.
- `Sources/tart/ControlSocket.swift` bridges a host Unix socket to the guest
  agent over `VZVirtioSocketDevice`.
- `Sources/tart/VMDirectory+OCI.swift` pulls and pushes VM config, disk, and
  NVRAM layers with Tart-specific media types.

The conclusion is that the VM mechanics are feasible. The hard work is fitting
them into Cleanroom's stronger sandbox contract.

## Replacement Boundary

"Replace Tart" should mean:

- Buildkite/macOS jobs no longer invoke the `tart` binary.
- Cleanroom owns clone, start, exec, file transfer or workspace mount,
  teardown, events, and diagnostics.
- Images are either Cleanroom-native macOS VM images or imported from an
  existing Tart-compatible image source.
- The user-facing API stays Cleanroom-shaped: repository policy, runtime
  config, capabilities, `cleanroom exec`, and the control API.

It should not mean:

- implement every Tart CLI flag
- implement Orchard-style orchestration
- support GUI-first workflows, VNC, audio, clipboard, or Screen Sharing in the
  first slice
- support Intel macOS guests or x86 emulation
- copy Tart source code or make Tart's image format the native Cleanroom
  contract

## Target Model

The public contract should eventually separate guest platform from host
backend. A possible future policy shape:

```yaml
sandbox:
  platform:
    os: macos
    arch: arm64
  image:
    ref: ghcr.io/buildkite/cleanroom/macos-sequoia-xcode@sha256:...
```

This is illustrative, not a schema proposal to implement in the first PR. The
important rule is that `macos` is a guest platform, not a new user-facing
backend. Backend-specific details stay in runtime config and adapter internals.

The first implementation does not need to land the final policy schema. It can
start with an experimental runner that reads a local bundle path, then graduate
to runtime config, and only then expose a repository policy surface once the
backend can enforce the advertised capabilities. That avoids documenting a
copyable `sandbox.platform` example before the loader and capability checks
exist.

The macOS VM image record should include:

- macOS version and build
- hardware model data
- minimum CPU and memory
- disk format
- guest-agent version and capabilities
- disk image digest
- auxiliary-storage digest
- optional source IPSW metadata

Each sandbox should clone the base disk and auxiliary storage into a
sandbox-owned bundle before boot. It should generate unique runtime identity
where Apple requires it, including MAC address and machine identifier handling,
and reject concurrent launches that would reuse unsafe identity.

### Local Bundle Contract

The first harness should accept a local bundle rather than OCI. The bundle is a
directory with a small metadata file and paths relative to that directory. A
strawman shape:

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

The exact fields can change after the first probe, but the runner should fail
closed when required identity, disk, auxiliary storage, or agent metadata is
missing. Keeping this metadata explicit gives the later OCI and image-import
work a concrete target instead of burying assumptions in filenames.

### Helper Boundary

The production helper should grow an explicit macOS start operation instead of
overloading the existing Linux `StartVM` request:

- `StartLinuxVM` or the current `StartVM` path keeps `kernel_path`,
  `rootfs_path`, Linux boot args, and Linux guest networking.
- `StartMacOSVM` takes bundle-derived disk, auxiliary storage, hardware model,
  machine identity policy, directory shares, socket-device port, and macOS
  start options.

The first benchmark harness can duplicate a small amount of Swift setup code to
learn quickly. The production integration should converge on one helper binary
so signing, entitlements, lifecycle operations, proxy sockets, and diagnostics
stay in one place.

## Guest Agent

The Linux `cleanroom-guest-agent` cannot be reused as-is because it owns Linux
init behavior, ext4 rootfs assumptions, and Linux service startup. macOS needs
a LaunchDaemon or LaunchAgent package installed into the image.

The first macOS agent should support:

- readiness probe
- non-interactive command execution
- stdout/stderr streaming
- stdin streaming
- environment and working directory
- exit code reporting
- path stat/read/write operations needed for workspace setup and artifact copy

TTY, clipboard, GUI session automation, keychain operations, and disk resize can
come later.

The host transport should prefer the existing virtio-socket pattern. If the
macOS agent can implement the existing `vsockexec` framing cleanly, the Go
control path stays smaller. If gRPC is faster to implement on macOS, hide it
behind the backend adapter and keep the control-service contract unchanged.

Tart's guest-agent design is useful evidence here: recent Tart images run
commands without SSH by using a Go guest agent and a bidirectional gRPC exec
stream over the VZ socket path. Cleanroom does not need to copy that protocol,
but it should copy the product boundary: runtime command execution must be a
guest-agent capability, not an SSH/network side channel.

## Networking

Networking is the biggest product-risk area.

Tart can use NAT, bridged networking, or Softnet. Cleanroom's value is stronger
than that: repo-scoped policy, deny-by-default egress, host-held credentials,
and gateway mediation.

The current `darwin-vz` filehandle gateway is the right long-term direction
because it already owns DNS, TCP proxying, gateway access, and policy swapping.
The missing macOS-guest piece is address configuration. Today the Linux guest
gets static network details through boot args. A macOS guest cannot use that
path. We need one of:

- DHCP support in the filehandle gateway
- a guest-agent network setup operation before workload execution
- a preconfigured static network profile in the base image, parameterized per
  sandbox

Using VZ NAT for the first boot/exec prototype is acceptable only if the
backend reports degraded capabilities and requires an explicit allow-all mode.
It should not be called a Cleanroom replacement for Tart until the filehandle
path can enforce the same policy model as Linux Cleanrooms.

## Image And Storage Strategy

There are two viable tracks:

1. Cleanroom-native macOS VM images.
   - Define Cleanroom media types for VM metadata, disk, and auxiliary storage.
   - Require digest-pinned refs for reproducible CI.
   - Use APFS clonefile or ASIF/raw sparse disk copy for fast local clones.
   - Keep image build/import tooling in Cleanroom.

2. Tart image import for migration.
   - Read Tart OCI media types as an importer.
   - Convert to a Cleanroom-native local bundle before execution.
   - Do not make Tart's format the long-term Cleanroom API.

The first prototype can skip OCI entirely by accepting a local VM bundle. That
keeps the first proof focused on boot, exec, and teardown.

## Current Progress Snapshot

Slice 1 is partially implemented in `benchmarks/darwin-vz/macos-minimal`.
Current evidence proves that the host and SDK expose the required
Virtualization.framework APIs, the existing Cleanroom `darwin-vz` backend has
reusable helper/control-plane pieces, Tart proves the macOS VM plus guest-agent
model works in practice, and the standalone macOS harness can compile and
validate bundle metadata paths. The benchmark directory also has a local
IPSW-to-bundle creator that installs macOS, writes VZ identity files, and emits
the runner's `bundle.json` shape.

The worktree now has a minimal `darwin/arm64` macOS guest agent command,
LaunchDaemon template, package builder, and offline prepare script that clones
a base bundle, mounts the clone's APFS Data volume, installs the agent, marks
setup complete, and updates `bundle.json`. The prepare script fails closed if
it cannot set root ownership, with an explicit inspection-only override for
rootless experiments. On the local macOS 26.5 build 25F71 bundle, metadata
validation succeeds after rootless preparation, but the live `sw_vers` smoke
still times out connecting to the guest vsock port. The rootless offline
install cannot set root ownership on the agent or LaunchDaemon plist, and the
guest did not create agent stdout/stderr logs during boot. The package builder
now produces `dist/cleanroom-macos-guest-agent.pkg` as a script-only installer:
its postinstall runs inside the guest as root, writes the agent and
LaunchDaemon plist as `root:wheel`, and bootstraps the LaunchDaemon when the
target is the running system. The next validation step is a setup boot or
privileged in-guest install that runs that package, then reruns the live smoke.

## Delivery Strategy

### Slice 1: Local macOS VM boot-and-exec probe

Add a standalone harness under `benchmarks/darwin-vz/macos-minimal`. This is
the macOS analogue of the existing Linux minimal benchmark: a learning tool,
not a backend.

Expected files:

- `benchmarks/darwin-vz/macos-minimal/README.md`
- `benchmarks/darwin-vz/macos-minimal/runner.swift`
- `benchmarks/darwin-vz/macos-minimal/build-runner.sh`
- `benchmarks/darwin-vz/macos-minimal/example-bundle.json`

Current status: these files exist. The runner builds and signs as
`dist/darwin-vz-macos-minimal`, `--help` works, and validation fails closed
when the example bundle points at missing artifacts or invalid
Virtualization.framework identity data. The repository Go suite also passes
locally with this harness present. Live VM validation is pending a prepared
macOS bundle with the guest agent installed.

Scope:

- read a local bundle metadata file and resolve relative disk/auxiliary paths
- build a `VZVirtualMachineConfiguration` with `VZMacOSBootLoader`,
  `VZMacPlatformConfiguration`, storage, socket device, and minimal console
  devices
- sign the runner with the existing virtualization entitlement
- start the VM without invoking `tart`
- connect to the guest agent over virtio socket
- run `/usr/bin/sw_vers` by default, with a flag for an arbitrary command
- stream stdout/stderr, return the exit code, and write machine-readable
  timing JSON to `--metrics` or stderr
- stop the VM cleanly

Definition of done:

- `--help` and bundle validation work without a VM bundle
- missing disk, auxiliary storage, hardware model, or agent metadata fails with
  a clear error
- live smoke works on an Apple Silicon host when
  `CLEANROOM_MACOS_VM_BUNDLE=/path/to/bundle.json` points at a prepared image
- Linux `benchmarks/darwin-vz/minimal` behavior is untouched

This slice may use VZ NAT only for image bring-up or debugging. The command
path itself should not depend on SSH or guest networking.

### Slice 2: macOS guest agent package

Add a minimal guest agent that can be installed into a macOS image. Prefer a
separate `cmd/cleanroom-macos-guest-agent` command at first so Linux init,
Docker, overlay-capture, and ext4 assumptions stay out of the macOS binary.

Scope:

- LaunchDaemon plist template for boot-time startup
- readiness RPC
- exec RPC with argv, env, working directory, stdin, stdout, stderr, and exit
  code
- path stat/read/write operations needed for workspace setup and artifacts
- version/capabilities RPC

Definition of done:

- agent builds for `darwin/arm64`
- LaunchDaemon starts the agent in a prepared image
- the Slice 1 runner can execute `sw_vers` and a shell command without SSH
- agent reports version and capability metadata to the host
- unsupported operations return explicit errors rather than silent success

Current status: `cmd/cleanroom-macos-guest-agent` builds for `darwin/arm64`,
serves the existing newline-delimited exec stream over stdio for tests and
Darwin AF_VSOCK in the guest, supports `ready`/`version` control requests, and
streams stdout, stderr, stdin EOF, environment, working directory, and exit
status. The LaunchDaemon template and package builder exist, but live launchd
startup is not yet proved because the current offline install path cannot set
root ownership in this non-sudo session.

### Slice 3: Image bundle creation and import

Create the smallest image workflow needed to repeat the boot-and-exec smoke
without hand-editing VM directories.

Scope:

- document a local bundle layout for disk, auxiliary storage, hardware model,
  machine identity policy, and agent metadata
- provide a local import or prepare command that validates an existing macOS VM
  bundle and writes normalized Cleanroom metadata
- install or verify the guest agent in the image
- clone the base bundle into a sandbox-owned working bundle before boot

Current status: `benchmarks/darwin-vz/macos-minimal/create-bundle.swift` and
`build-create-bundle.sh` can build a signed installer helper that accepts a
local Apple Silicon IPSW, creates `disk.img`, `auxiliary.storage`,
`hardware-model.bin`, `machine-identifier.bin`, runs `VZMacOSInstaller`, and
writes `bundle.json`. The tool intentionally stops short of claiming the bundle
is command-runnable because the Cleanroom macOS guest agent still needs to be
installed inside the guest.

`prepare-agent-bundle.sh` now clones the base bundle, installs the local macOS
guest agent into the clone's APFS Data volume, writes the LaunchDaemon plist,
marks setup complete, and updates `bundle.json` to the installed agent version.
It leaves the base bundle untouched and fails closed when root ownership cannot
be set, unless the caller passes the inspection-only rootless override.
Rootless offline installation has not produced a launchd-started agent yet.
`build-guest-agent-pkg.sh` creates a script-only installer package for an
in-guest finalization path. The package avoids host-side AppleDouble payload
entries and uses postinstall to write root-owned files, then tries to bootstrap
and kickstart the LaunchDaemon.

Definition of done:

- repeated local runs clone from the same base without mutating it
- concurrent runs cannot reuse unsafe identity state
- bundle validation rejects missing or mismatched metadata before VM start
- clone time and disk usage are measured on APFS

### Slice 4: Experimental backend integration

Wire the macOS guest path into `darwin-vz` behind explicit experimental
configuration and capability flags. The first runtime config should stay
operator-scoped; repository policy should not advertise macOS guests until
capability checks and validation are in place.

Possible runtime config:

```yaml
backends:
  darwin-vz:
    macos:
      enabled: true
      bundle: /var/lib/cleanroom/macos-images/sequoia-xcode/bundle.json
```

Scope:

- add a macOS-specific helper operation such as `StartMacOSVM`
- add backend adapter code that selects the macOS path only for explicit
  experimental macOS requests
- keep `ProvisionSandbox`, `RunInSandbox`, and `TerminateSandbox` as the public
  backend interface
- publish separate capability gaps for macOS guest support in
  `cleanroom doctor --json`
- fail closed when a policy or command needs unsupported capabilities

Definition of done:

- normal Linux `darwin-vz` behavior and tests are unchanged
- an experimental macOS sandbox can run a command through Cleanroom control
  flow without Tart
- unsupported file, network, snapshot, cache-output, or Docker capabilities
  fail before execution
- observability includes guest platform, image metadata, startup timings, and
  agent version

### Slice 5: Policy-compatible networking

Extend the filehandle gateway or guest provisioning path so macOS guests use a
Cleanroom-owned network path.

Scope:

- decide between DHCP in the filehandle gateway, guest-agent network setup, or
  preconfigured static profiles
- ensure DNS, TCP proxying, gateway access, and stage policy swaps behave like
  Linux `darwin-vz`
- close active TCP proxy connections when the active policy changes

Definition of done:

- deny-by-default policy blocks arbitrary TCP egress
- allowed hosts work through the existing DNS/TCP authorization model
- stage-scoped policy swaps close active TCP proxy connections
- host gateway access works through a stable name
- conformance tests cover blocked and allowed traffic

### Slice 6: Image lifecycle and Buildkite migration

Add enough image lifecycle to replace the Tart Buildkite plugin for the target
workflow.

Scope:

- choose Cleanroom-native image publishing as the product path
- add Tart image import only as migration tooling if existing Cirrus/Tart
  images are required
- publish or import a prebuilt Xcode image with the Cleanroom guest agent
- run a Buildkite command inside the VM without invoking `tart`
- capture artifacts and workspace writes through the chosen copy/mount model

Definition of done:

- the target Buildkite job runs with no `tart` binary in the command path
- startup, clone time, command runtime, cleanup time, and disk usage are
  measured against the current Tart workflow
- logs and failure diagnostics are good enough to operate in CI
- remaining capability gaps are visible in `doctor --json` and user-facing
  errors

## Verification Plan

- Document/static checks: `git diff --check` and a README smoke for every new
  command or bundle format.
- Swift compile/signing: `xcrun swiftc -framework Virtualization` and
  `codesign --entitlements cmd/cleanroom-darwin-vz/entitlements.plist` for the
  experimental runner and helper changes.
- Host support check: fail early when `VZVirtualMachine.isSupported` is false,
  the host is not Apple Silicon, or required macOS guest APIs are unavailable.
- Bundle validation tests: missing disk, auxiliary storage, hardware model,
  unsupported arch, missing agent port, and unsafe identity reuse.
- Agent tests: exec success, non-zero exit, stdin/stdout/stderr streaming,
  env/working directory handling, readiness, and unsupported operation errors.
- Backend unit tests: Linux `darwin-vz` path remains selected by default,
  experimental macOS config is required, capability gaps are reported, and
  unsupported policy features fail closed.
- Live smoke: with `CLEANROOM_MACOS_VM_BUNDLE` set, start a macOS VM, run
  `/usr/bin/sw_vers`, run a shell command that writes an artifact, and stop the
  VM cleanly.
- Network conformance: after Slice 5, run allowed/blocked TCP and stage-scoped
  policy tests against the macOS guest path before claiming Tart replacement
  parity.

## Key Learnings From Pressure-Testing

Booting macOS is not the hard part. The risky parts are the Cleanroom-specific
parts Tart does not solve: policy-grade networking, host-side credential
mediation, deterministic image identity, and a backend-neutral control API.

Treating VZ NAT plus SSH as "macOS Cleanrooms" would be too weak. It may prove
that a VM can boot, but it does not prove Cleanroom can replace Tart for a
policy-governed sandbox.

Image management can become the largest maintenance burden. If the actual goal
is only to run existing Tart images, an importer is pragmatic. If the goal is a
Cleanroom product surface, use a Cleanroom-native image format and keep Tart
compatibility as migration tooling.

The first PR should not modify the production `darwin-vz` Linux path. A small
standalone harness gives us a fast way to learn about macOS guest boot,
virtio-socket agent behavior, and image bundle metadata without destabilizing
the existing backend.

## Resolved Decisions And Defaults

- First replacement target: Buildkite-style macOS CI command execution. Local
  developer sandboxes and GUI workflows can benefit later, but they should not
  shape the first supported surface.
- First PR boundary: standalone `benchmarks/darwin-vz/macos-minimal` harness,
  not production backend integration.
- Runtime dependency: no `tart` binary in the execution path. Temporary use of
  Tart-created images is acceptable only as input to an importer or local
  bundle migration step.
- Command transport: guest agent over virtio socket, not SSH.
- First probe protocol: keep the existing `vsockexec` newline-delimited JSON
  shape for exec, with small `ready` and `version` control requests for the
  macOS agent.
- Networking default: VZ NAT is acceptable for a local boot probe, but not for
  claimed Cleanroom parity.
- Public shape: guest platform belongs in policy or request shape; backend
  selection remains runtime/operator configuration.
- Image direction: prefer Cleanroom-native metadata and digests; add Tart import
  as migration tooling if it materially reduces adoption cost.

## Open Questions

Questions that block Slice 1:

- What installation path should make the LaunchDaemon acceptable to launchd?
  Recommended default: run the generated agent package during a setup boot or
  other privileged in-guest finalization step, then rerun the existing
  `sw_vers` smoke.

Questions before backend integration:

- Should repository policy grow `sandbox.platform` directly, or should the
  first integrated path use an internal request/runtime flag? Recommended
  default: runtime flag first, policy schema only after capability checks are
  enforceable.
- How should macOS guest identity be regenerated or fenced per sandbox?
  Recommended default: require clone-time identity handling in the bundle tool
  and reject concurrent runs if safety cannot be proven.

Questions before Tart replacement parity:

- Do we need to run existing Cirrus/Tart images directly, or is building new
  Cleanroom macOS images acceptable?
- What macOS image build pipeline owns Xcode/base image updates?
- What legal and capacity constraints should the scheduler enforce for macOS
  guests on shared Apple hardware?
