# Firecracker Privilege Reduction and Host Runtime Plan

**Status:** Proposed
**Scope:** Linux `firecracker` backend first

## Summary

Cleanroom should stop treating Firecracker privilege as a generic `sudo` escape
hatch. The current Linux backend already launches the Firecracker process as the
calling user, but it still relies on a root-owned helper for two different
classes of work:

1. host networking and gateway firewall changes
2. loop-mounting ext4 images to inject the guest runtime

Those two classes have different requirements and should not share the same
mechanism.

This proposal splits the problem into two tracks:

- **Track A: CI-friendly privilege reduction**
  Remove root from runtime rootfs preparation entirely and make CI use the same
  Linux host runtime as production.
- **Track B: production host runtime**
  Replace ad-hoc privileged operations with a root-owned host daemon that
  manages
  network leases, gateway firewall state, optional snapshot backends, and
  Firecracker `jailer` launches.

Near-term release work is intentionally narrower than this full plan. The
current supported Linux story still uses the helper-based machine bootstrap.
That means:

- the helper, sudoers, KVM access, and related host commands are machine-level
  prerequisites
- runtime config remains a user-level choice layered on top of that bootstrap
- ZFS-backed Firecracker is the supported layered-cache path
- file-backed Firecracker remains functional but degraded for warm restores
- `cleanroom doctor` is the source of truth for the current support tier on a
  host while the broader `hostd` plan remains future work

The user-facing CLI and API stay backend-neutral. Backend-specific privilege
details remain in adapter internals and a small Linux host-runtime convention.
The target is one supported Linux runtime model with good defaults.
Activation path is a separate concern:

- **persistent activation** via `cleanroom daemon install` on Linux
- **transient activation** via interactive `cleanroom serve` bootstrapping
  `hostd` once with sudo when the standard socket is missing

Both paths lead to the same `hostd + jailer` runtime.

## Why change this now

Current state:

- Firecracker itself is launched directly by the backend as the current user.
- The privileged helper is invoked via `sudo -n`.
- The helper contract is command-shaped (`ip`, `iptables`, `mount`, `install`,
  `sysctl`, `zfs`) rather than resource-shaped.
- CI hosts must install a root-owned helper out of band and keep it in sync
  with backend expectations.
- Runtime rootfs preparation still requires loop mounts even though the
  repository already has an unprivileged ext4 mutation path for `darwin-vz`
  using `debugfs`.

Problems with the current shape:

- **CI is noisier than it needs to be.** Passwordless sudo is currently needed
  for both real network privilege and avoidable rootfs mutation.
- **Helper drift is operationally awkward.** Every new helper capability or
  argv shape is a host rollout dependency.
- **The root surface is too low-level.** A shell-style allowlist of raw host
  commands is harder to reason about than typed operations like "ensure sandbox
  network" or "ensure gateway firewall."
- **Production isolation is incomplete.** Running Firecracker as a non-root
  user is good, but a production host still wants a root-owned control plane
  for network state, cleanup, and `jailer`-based privilege dropping.

## Goals

- Keep the Firecracker VMM non-root by default.
- Eliminate root from runtime rootfs preparation.
- Reduce CI sudo requirements to the smallest practical surface.
- Provide a production-grade privileged architecture that fits Cleanroom's
  source-IP identity and gateway model.
- Keep top-level CLI and control-plane APIs unchanged.
- Converge CI and production on one supported Linux host runtime model.
- Prefer convention and fixed install locations over path-heavy runtime config.
- Keep backend-specific runtime details in adapter internals and host services.
- Preserve a local-development `cleanroom serve` path that still "just works"
  without making sudo-per-operation part of the supported runtime design.
- Make CI use persistent host-runtime installation and avoid interactive sudo in
  jobs.
- Make privilege checks and operator expectations visible in `cleanroom doctor`.

## Non-goals

- Adding a rootless Linux backend in this plan.
- Changing policy semantics, gateway semantics, or snapshot semantics.
- Introducing backend-specific user-facing commands.
- Moving Firecracker sandboxes into separate network namespaces in the first
  production slice. Cleanroom's current host gateway model depends on host-side
  TAP identity.

## Current privilege surface

| Area | Current mechanism | Root required today | Target |
|---|---|---:|---|
| `/dev/kvm` access | delegated device access | no | keep non-root |
| Firecracker launch | direct process exec | no | `hostd`-managed `jailer` launch |
| Runtime rootfs injection | loop mount + install | yes | eliminate root |
| Per-sandbox TAP + policy rules | helper + `ip`/`iptables`/`sysctl` | yes | `hostd`-managed network lease |
| Gateway firewall | helper + `iptables` | yes | `hostd`-managed gateway firewall |
| ZFS snapshot plumbing | helper + `zfs` | yes | optional `hostd` storage ops |

## Design principles

### 1. Root should be a host service, not a shell escape

The root boundary should expose typed operations with stable contracts:

- ensure or release sandbox network
- ensure or release gateway firewall
- launch or stop jailed microVM
- perform optional host-native snapshot operations

It should not expose a growing list of raw `iptables`, `mount`, and `install`
argv patterns.

### 2. Unprivileged work stays unprivileged

If a task can be done by the normal control process, it should be. Runtime
rootfs preparation is the clearest example: the repository already proves that
ext4 mutation can be done without root by using `debugfs` on `darwin-vz`.

### 3. Keep the public UX stable

Users should continue using:

- `cleanroom exec`
- `cleanroom sandbox create`
- `cleanroom console`
- `cleanroom serve`
- `cleanroom doctor`

Linux host-runtime selection should not become a new execution mode exposed
through every command.

### 4. Keep the host-runtime boundary typed

The Firecracker adapter should depend on an internal host-runtime client
interface rather than on shell commands or sudo wrappers.

There should be one supported implementation of that interface on Linux:

- a root-owned daemon over a Unix socket

Tests can use fakes. CI and production should use the same runtime.

### 5. Prefer convention over config for host runtime

The Linux host runtime should use fixed defaults unless there is a strong reason
not to:

- `cleanroom-hostd` Unix socket at `/run/cleanroom/hostd.sock`
- host-owned runtime state under `/run/cleanroom`
- host-owned durable state under `/var/lib/cleanroom`
- `jailer` resolved from `PATH`

If a path exists only to support one installation layout, it should not be a
stable runtime-config key.

### 6. Separate activation path from runtime design

Linux can support two ways to reach the same runtime without turning them into
two different privilege modes:

- a persistent host runtime installed once by root
- a transient host runtime started on demand for interactive local development

The runtime itself remains the same in both cases: `cleanroom-hostd` owns
privileged operations and Firecracker launches through `jailer`.

## Proposal

## Track A: CI-friendly privilege reduction

### A1. Remove root from runtime rootfs preparation

Firecracker should adopt the same ext4 mutation model already used by
`darwin-vz`:

- create or derive the ext4 image unprivileged
- inject `cleanroom-guest-agent`
- inject `cleanroom-init`
- create directories and replace files using `debugfs`

Implementation direction:

- Extract the ext4 edit helpers out of `internal/backend/darwinvz` into a
  shared package such as `internal/ext4runtime` or `internal/ext4edit`.
- Update the Firecracker backend to use that shared implementation instead of
  loop-mounting with root.
- Add a Firecracker `doctor` check for `debugfs`, mirroring the existing
  `darwin-vz` check.

Benefits:

- Removes `mount`, `umount`, `mkdir`, and `install` from the privileged path.
- Removes the most awkward repo-artifact-to-rootfs copy path from CI.
- Shrinks helper churn and host rollout pressure immediately.

### A2. Replace raw helper commands with typed host-runtime operations

After rootfs prep is unprivileged, the remaining privileged Linux work is
network, optional ZFS, and Firecracker launch. That path should move behind a
typed internal client backed by `cleanroom-hostd`.

Proposed internal interface:

```go
type HostRuntime interface {
    Version(ctx context.Context) (string, error)
    Capabilities(ctx context.Context) ([]string, error)

    EnsureGatewayFirewall(ctx context.Context, req GatewayFirewallRequest) (Lease, error)
    EnsureSandboxNetwork(ctx context.Context, req SandboxNetworkRequest) (SandboxNetworkLease, error)
    ReleaseLease(ctx context.Context, leaseID string) error

    ZFSCreateSnapshot(ctx context.Context, req ZFSCreateSnapshotRequest) error
    ZFSDestroySnapshot(ctx context.Context, req ZFSDestroySnapshotRequest) error

    LaunchMicroVM(ctx context.Context, req LaunchMicroVMRequest) (MicroVMHandle, error)
    StopMicroVM(ctx context.Context, handleID string) error
}
```

`SandboxNetworkRequest` should be resource-shaped, not command-shaped. It should
carry:

- sandbox ID
- tap name
- host IP / guest IP
- gateway port
- resolved allowlist destinations
- whether IPv6 must be disabled

The request should represent the desired network state. The privileged side owns
how that becomes iptables or nftables state.

### A3. Remove the helper as a supported runtime component

The helper should not survive this redesign as a supported runtime path.

Consequences:

- no Linux runtime mode for `sudo-helper`
- no helper path configuration
- no helper capability matrix in long-term docs
- no branch-to-host helper drift as an operational concern
- no repeated `sudo` calls in steady-state operation

CI host expectations become:

- Firecracker binary installed
- `buildkite-agent` or runtime user in the `kvm` group
- `mkfs.ext4` and `debugfs` available
- `cleanroom-hostd` installed and running at the standard socket
- `jailer` installed and discoverable in `PATH`

`cleanroom doctor` should report:

- privilege runtime
- host runtime version
- whether rootfs prep is unprivileged
- whether `jailer` is available
- whether the connected host runtime is transient or persistent

### A4. Use two activation paths into the same runtime

Linux should support two operator-facing paths into `hostd`:

1. **Persistent activation**
   `cleanroom daemon install` installs and enables the standard host runtime.
2. **Transient activation**
   `cleanroom serve` can bootstrap `hostd` once with sudo when the standard
   socket is absent and the terminal is interactive.

Rules:

- both paths use the same socket path and host-runtime contract
- both paths use the same `hostd` implementation
- neither path reintroduces raw helper passthrough or per-command sudo
- transient activation is for local development only
- non-interactive contexts must not attempt transient bootstrap

## Track B: Production host runtime

### B1. Add a root-owned host daemon

Production should not use repeated `sudo` invocations from the unprivileged
control service. Instead, introduce a root-owned daemon, `cleanroom-hostd`,
with a Unix socket transport.

Responsibilities:

- own gateway firewall state
- own per-sandbox TAP and egress rule state
- own cleanup and reconciliation after crashes or restarts
- own optional ZFS-backed snapshot operations
- launch Firecracker through `jailer` in production mode

Non-responsibilities:

- loading repository policy files
- compiling policy
- choosing commands to execute in the guest
- reading repository checkout state directly

The unprivileged control service remains the policy authority. `hostd` receives
typed, already-validated requests and executes only host-runtime actions.

`hostd` should be the default and only supported Linux privilege runtime. CI
and production should use the same host runtime model.

### B2. Reconcile network state as owned resources

`hostd` should manage named owned resources rather than append ad-hoc rules to
global chains.

Recommended model:

- create dedicated Cleanroom-owned INPUT/FORWARD/NAT chains, or an nftables
  table with equivalent semantics
- tag each sandbox network lease with a durable lease ID
- store lease metadata under `/run/cleanroom` and durable host-owned state
  under `/var/lib/cleanroom`
- reconcile owned state on startup and garbage-collect orphaned TAP devices or
  stale firewall entries

Backend choice:

- For the first daemon slice, reusing the current iptables semantics is
  acceptable if the daemon owns dedicated chains and can reconcile them.
- nftables may be a better long-term backend, but it should be an internal
  daemon detail, not a new user-facing contract.

### B3. Launch Firecracker through `jailer`

Production mode should launch Firecracker through upstream `jailer` rather than
spawning the VMM directly from the unprivileged Cleanroom service.

Proposed shape:

- `cleanroom serve` runs as an unprivileged service user
- `hostd` receives a typed launch request
- `hostd` prepares a jail root under a host-owned base directory
- `hostd` allocates or derives a per-sandbox uid/gid
- `hostd` invokes `jailer`
- `jailer` drops privileges before Firecracker starts executing as the jailed
  user

Initial production scope should include:

- jailed root directory layout
- per-sandbox uid/gid allocation
- cgroup ownership by `hostd`
- clean teardown when the sandbox is terminated

This improves:

- privilege dropping
- filesystem containment
- process ownership clarity
- host cleanup after control-plane crashes

### B4. Do not add netns in the first production slice

It is tempting to fold network namespaces into the same change. We should not do
that in the first pass.

Reason:

- Cleanroom's current gateway identity model depends on host-visible TAP and
  guest source IP identity.
- Moving interfaces into separate namespaces changes the reachability model for
  the gateway and complicates the current source-IP-based routing design.

The first production slice should keep the current host namespace model and add:

- a proper network manager
- owned firewall state
- `jailer`

That is enough to materially improve production posture without reworking the
gateway architecture.

## Runtime config and UX

### Public UX

No new top-level user command surface is required.

The main UX changes should be:

- `cleanroom config init` does not write Linux privilege paths or mode toggles
- `cleanroom doctor` shows the detected Linux privilege runtime and
  prerequisites
- `cleanroom serve` logs the detected Linux privilege runtime once at startup
- `cleanroom daemon install` on Linux installs the host runtime bundle rather
  than a global long-running control-plane daemon by default

### `serve` ergonomics on Linux

`cleanroom serve` should stay unprivileged on Linux.

Startup flow:

1. attempt to connect to `/run/cleanroom/hostd.sock`
2. if the socket is available, continue normally
3. if the socket is missing and the terminal is interactive, offer to start a
   transient `hostd` with sudo
4. wait for the socket to become ready, then continue
5. if the terminal is non-interactive, fail fast with an actionable error

Important invariants:

- `serve` itself does not become root
- only `hostd` receives elevated privileges
- transient bootstrap is not attempted in CI or other non-interactive runs
- all sandbox network, firewall, and jailed Firecracker operations still go
  through `hostd`

This preserves a local "serve just works" experience without reintroducing the
old helper architecture.

### `daemon install` semantics on Linux

On Linux, `cleanroom daemon install` should primarily mean "install the host
runtime bundle", not "install a shared long-running `cleanroom serve` process."

Recommended installed units:

- `cleanroom-hostd.socket`
- `cleanroom-hostd.service`

Recommended responsibilities of `daemon install`:

- install the systemd units
- create or validate `/run/cleanroom` and `/var/lib/cleanroom`
- ensure the service account and permissions are correct
- enable the socket so `hostd` is available before any unprivileged `serve`
  process starts

Linux `daemon install` should not, by default, install a global
`cleanroom.service`. The control plane is better treated as job-local or
user-local state, while `hostd` is the machine-local privileged runtime.

If operators want a shared long-running `cleanroom serve`, that should be a
separate advanced deployment choice rather than the default Linux install
behavior.

### CI model

CI should use the persistent host-runtime path only.

Recommended CI shape:

1. host bootstrap once as root:
   - install `cleanroom-hostd`
   - install `firecracker`, `jailer`, `mkfs.ext4`, and `debugfs`
   - add the CI user to `kvm`
   - run `cleanroom daemon install`
2. each job starts its own unprivileged `cleanroom serve`
3. each job uses isolated XDG runtime, config, data, and state directories
4. jobs connect to their own local control socket
5. all privileged work flows to the already-installed `hostd`

Why this shape is preferred:

- no interactive sudo in jobs
- no helper rollout drift
- per-job control-plane isolation for config, credentials, and logs
- one machine-local privileged runtime with restart and cleanup ownership

`cleanroom serve` should refuse transient bootstrap in non-interactive CI runs.
If `hostd` is unavailable, it should fail with a clear message that the host
runtime is not installed or not running.

### Runtime config

Recommended new Firecracker-specific runtime config:

```yaml
backends:
  firecracker:
    binary_path: firecracker
    kernel_image: ""
```

Notes:

- This can be a breaking config change; the project is early and does not need
  legacy compatibility unless explicitly requested.
- `privileged_helper_path` should be removed rather than carried forever.
- `hostd_socket`, `helper_path`, `jailer_base_dir`, and similar host-runtime
  paths should not be normal runtime-config keys.
- `cleanroom-hostd` should own its standard socket and state locations by
  convention.
- `jailer` should be on by default for Linux Firecracker once `hostd` launches
  VMs; lack of `jailer` should be a failing prerequisite, not a preference knob.
- transient bootstrap should not be a runtime-config mode; it is an interactive
  `serve` behavior when the standard socket is missing

The long-term design should bias toward:

- no Linux privilege mode setting
- no path configuration for helper or daemon sockets
- no runtime flag to disable `jailer` in supported Firecracker deployments
- no runtime flag to choose between transient and persistent host-runtime
  activation

### Doctor output

Recommended additional checks:

- `privilege_runtime`
- `hostd_activation`
- `debugfs`
- `hostd_socket`
- `jailer_binary`
- `kvm_group_access`
- `network_backend` (informational)

Recommended warning behavior:

- fail if host runtime version is below the required contract
- fail if `hostd` is unavailable for supported Linux Firecracker use
- fail if `jailer` is unavailable once the daemonized launch path lands
- warn when connected to a transient `hostd` instance

## Architecture fit with the current codebase

This proposal fits the current repository direction:

- backend-neutral CLI and API stay unchanged
- backend-specific host runtime details remain in the Firecracker backend and
  host services
- the existing gateway identity model stays intact
- `darwin-vz` already provides a working unprivileged ext4 mutation pattern
  that Firecracker can reuse

Suggested internal package split:

- `internal/ext4runtime` or `internal/ext4edit`
  Shared ext4 mutation logic used by both `darwin-vz` and Firecracker.
- `internal/hostruntime`
  Typed request and response structs plus client interface.
- `internal/hostruntime/client`
  Unix-socket client for the supported Linux runtime.
- `internal/hostd`
  Root-owned daemon and state reconciliation logic.

The Firecracker adapter should depend on `hostruntime.Client`, not on helper
paths or raw command wrappers. The adapter should not own path resolution for
the supported host runtime.

## Implementation slices

### Slice 1: unprivileged rootfs prep

- Extract shared ext4 mutation helpers from `darwin-vz`.
- Switch Firecracker runtime rootfs preparation to `debugfs`.
- Remove rootfs-related privileged-runtime assumptions from the Firecracker
  backend.
- Update Firecracker doctor checks and docs.

Definition of done:

- Firecracker no longer calls the helper for `mount`, `umount`, `mkdir`, or
  `install`.
- Linux Firecracker requires `debugfs` for runtime rootfs preparation.

### Slice 2: typed host-runtime client and daemon

- Add `internal/hostruntime`.
- Add `internal/hostd`.
- Replace `runRootCommand`-style call sites with typed requests to `hostd`.
- Add Unix-socket client wiring to the Firecracker backend.

Definition of done:

- Firecracker adapter does not build raw `iptables`, `ip`, or `sudo` helper
  argv directly.
- Linux Firecracker uses `hostd` at the standard socket.

### Slice 3: network and gateway ownership in `hostd`

- Implement gateway firewall and sandbox network lifecycle in the daemon.
- Add lease persistence and startup reconciliation.

Definition of done:

- Firecracker uses `hostd` for network setup and teardown.
- Owned network state is reconciled after restarts.

### Slice 4: CLI integration and activation paths

- Update `cleanroom serve` to connect to the standard `hostd` socket.
- Add interactive transient bootstrap for local Linux `serve` when the socket is
  missing.
- Make transient bootstrap unavailable in non-interactive contexts.
- Change Linux `cleanroom daemon install` to install the `hostd` bundle rather
  than a global `cleanroom serve` unit.
- Update `daemon status` to report host-runtime bundle health on Linux.

Definition of done:

- local `cleanroom serve` can bootstrap `hostd` once with sudo when needed
- CI/non-interactive `cleanroom serve` fails fast instead of prompting
- Linux `daemon install` manages `cleanroom-hostd.socket` and
  `cleanroom-hostd.service`

### Slice 5: `jailer` launch path

- Add `hostd` support for jailed Firecracker launches.
- Make `jailer` the supported Linux Firecracker launch path once `hostd` owns
  VM launch.
- Add doctor checks and operator docs for production mode.

Definition of done:

- Supported Linux Firecracker launches through `jailer`.
- Cleanroom control service does not directly spawn Firecracker in supported
  Linux runtime mode.

## Recommended rollout

Order matters:

1. Eliminate rootfs privilege first.
2. Introduce the typed host-runtime interface and `hostd`.
3. Move network and gateway ownership into `hostd`.
4. Add `serve`/`daemon install` integration around the standard `hostd`
   runtime.
5. Add `jailer` as the default launch path once daemonized launch exists.

This gives immediate CI wins without blocking on the larger production runtime.

## Recommendation

Adopt both tracks, but sequence them deliberately:

- **Near term:** make Firecracker rootfs preparation unprivileged and introduce
  `hostd` as the Linux host runtime.
- **Long term:** make `hostd + jailer` the supported Linux Firecracker runtime
  with no helper-based fallback.

That preserves Cleanroom's existing UX, respects the repository's
backend-neutral design goals, and gives a practical path from today's CI setup
to a materially stronger Linux host runtime.
