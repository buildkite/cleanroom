# Snapshot, Restore, and Fork Plan

**Status:** Proposed
**Scope:** Firecracker first, `darwin-vz` later

## Summary

Add three new environment primitives:

- `snapshot`: capture an immutable cleanroom filesystem state.
- `restore`: reset an existing sandbox back to a snapshot.
- `fork`: create a new sandbox from a snapshot.

The user-visible contract is intentionally filesystem-centric, not live-VM-centric.
Every `restore` and `fork` results in a fresh VM boot from the captured state.
That keeps the primitive deterministic, safe to compose, and compatible with
Cleanroom's existing network identity and guest-agent model.

On Firecracker, the implementation uses a new copy-on-write volume store with a
`zfs` driver first. A snapshot is a host-side COW snapshot of the sandbox root
volume. A fork is a COW clone of that snapshot. Restore replaces the sandbox's
mutable volume with a fresh clone of the snapshot and boots again.

This gives us a strong base for CI:

1. start from a deterministic image-backed cleanroom,
2. run bootstrap work once (`git clone`, dependency install, generated fixtures),
3. snapshot the result,
4. fork many isolated sandboxes from that golden state.

## Why Fresh-Boot Snapshots

The tempting alternative is to expose Firecracker's full VM snapshot/restore as
the primary primitive. We should not do that.

Reasons:

- Cleanroom cares about reusable environment state, not duplicated process state.
- Fresh boot gives every fork a new sandbox ID, guest IP, gateway registration,
  and vsock endpoint.
- We avoid cloning open TCP connections, old vsock sessions, wall-clock drift,
  and any in-memory uniqueness bugs from resumed workloads.
- Firecracker snapshot load requires the original disk/vsock resources to appear
  at the same paths, which complicates concurrent forks materially.
- Firecracker's own snapshot docs call out clone networking, vsock reset, and
  uniqueness concerns; those are acceptable as an optimization layer, but they
  are the wrong default semantics for CI primitives.

The result is a simpler contract:

- snapshot captures disk state only,
- restore/fork always boot a new VM from that disk state,
- any later Firecracker resume fast-path is an internal optimization, not a
  change in user-facing semantics.

## Goals

- Capture a golden cleanroom state after deterministic setup work.
- Make `fork` and `restore` near-constant-time at the storage layer.
- Keep the CLI, API, and core capability surface backend-neutral even if only
  Firecracker implements it first.
- Preserve existing policy immutability and gateway security invariants.
- Allow snapshot lineage: a forked sandbox can create later snapshots.
- Keep snapshots durable across daemon restarts.
- Allow the guest workload to mark checkpoint-safe moments cooperatively.

## Non-Goals

- `darwin-vz` support in the first implementation.
- Exposing live process or in-memory checkpoint semantics to users.
- Cross-backend snapshot portability.
- Restoring a snapshot under a different compiled policy hash.
- Using Firecracker diff snapshots as a required mechanism.

## Primitive Model

### Snapshot

An immutable environment artifact derived from a `READY` sandbox.

Snapshot metadata:

- `snapshot_id`
- `backend`
- `policy_hash`
- `image_ref`
- `image_digest`
- `parent_snapshot_id` (optional)
- `source_sandbox_id`
- `created_at`
- `labels` or `name` (optional)
- storage reference (`volume_snapshot_ref`)

Semantics:

- Captures the writable root volume only.
- Does not capture running processes, sockets, network identity, or active execs.
- Can be used as the parent for many forks.
- Can be the parent of later snapshots, creating a lineage graph.

### Fork

Provision a new sandbox from a snapshot.

Semantics:

- New sandbox ID.
- New guest IP / TAP / gateway registration.
- New writable child volume cloned from the snapshot.
- Fresh VM boot.
- Same compiled policy and backend lineage as the snapshot.

### Restore

Reset an existing sandbox to a snapshot.

Semantics:

- Same sandbox ID.
- Existing mutable volume is discarded.
- A fresh writable clone of the snapshot replaces it.
- Existing VM is terminated and a fresh VM boots.
- Sandbox returns to `READY` with the same control-plane identity, but no
  process state survives the restore.

## API Surface

Keep the control plane backend-neutral. Backend-specific storage details live in
runtime config and the Firecracker adapter.

### New service

Add `SnapshotService`:

```proto
service SnapshotService {
  rpc CreateSnapshot(CreateSnapshotRequest) returns (CreateSnapshotResponse);
  rpc GetSnapshot(GetSnapshotRequest) returns (GetSnapshotResponse);
  rpc ListSnapshots(ListSnapshotsRequest) returns (ListSnapshotsResponse);
  rpc DeleteSnapshot(DeleteSnapshotRequest) returns (DeleteSnapshotResponse);
}
```

### Sandbox creation from snapshot

Extend `CreateSandboxRequest` to allow a snapshot source:

```proto
message CreateSandboxRequest {
  string backend = 2;
  SandboxOptions options = 3;
  Policy policy = 4;

  oneof source {
    string snapshot_id = 5;
  }
}
```

Rules:

- when `snapshot_id` is set, `policy` must be omitted
- backend and policy are derived from the snapshot metadata
- any mismatch fails closed

### Restore

Add a sandbox lifecycle RPC:

```proto
rpc RestoreSandbox(RestoreSandboxRequest) returns (RestoreSandboxResponse);
```

```proto
message RestoreSandboxRequest {
  string sandbox_id = 1;
  string snapshot_id = 2;
}
```

### CLI shape

Proposed commands:

- `cleanroom snapshots create <sandbox-id> [--name <name>]`
- `cleanroom snapshots get <snapshot-id>`
- `cleanroom snapshots list`
- `cleanroom snapshots delete <snapshot-id>`
- `cleanroom sandboxes create --from-snapshot <snapshot-id>`
- `cleanroom sandboxes restore <sandbox-id> --snapshot <snapshot-id>`

Useful sugar later:

- `cleanroom exec --from-snapshot <snapshot-id> --rm -- <command>`

### Guest-cooperative checkpoint trigger

For CI and bootstrap flows, the guest should be able to request a snapshot at a
known-safe point.

Current guest runtime fact:

- Cleanroom already injects `cleanroom-guest-agent` into guest images.
- Cleanroom does not currently inject a user-facing `cleanroom` CLI inside the
  guest.

Recommendation:

- extend `cleanroom-guest-agent` with a user-invokable subcommand such as
  `cleanroom-guest-agent checkpoint request --name <name>`
- do not add a second guest control binary in v1

Semantics:

- the guest can request a checkpoint
- the host remains authoritative for whether and when the snapshot is taken
- the request is advisory until the sandbox reaches a safe point

Safe-point rules:

- if there is an active execution, the request becomes `pending`
- the host should normally honor the request only after the triggering
  execution exits cleanly and the sandbox is idle
- mid-execution freeze is not the default behavior

Why:

- CI bootstrap steps often know the exact moment when the environment becomes a
  useful golden state
- the host must still preserve the trust boundary because workload code inside
  the sandbox is untrusted

### Checkpoint request protocol

Recommended flow:

1. workload inside the guest invokes `cleanroom-guest-agent checkpoint request`
2. guest agent sends a checkpoint request to the host control plane
3. control plane records the request against the sandbox, with optional
   metadata such as name, labels, and requested mode
4. once the sandbox is idle, the control plane asks the guest agent to run a
   quiesce hook
5. after successful quiesce, the host pauses the VM and creates the snapshot
6. control plane records the resulting snapshot metadata and clears the pending
   request

Quiesce hook responsibilities:

- run `sync`
- optionally stop or flush known daemons
- optionally emit metadata describing the checkpoint

The quiesce step is guest-cooperative; the snapshot itself is host-executed.

### Guest-to-host transport

Recommendation for v1:

- use a small control endpoint on the existing host-side control/gateway path
  reachable from the guest network
- do not require a new host-side vsock listener just for checkpoint requests

Rationale:

- Cleanroom already uses guest networking plus host mediation for other control
  surfaces
- host-side vsock listening is operationally more awkward than reusing the
  existing mediated path
- the actual snapshot operation still happens entirely on the host

### Optional API addition

We can expose checkpoint requests explicitly in the control plane, separate from
immediate snapshot creation:

```proto
rpc RequestCheckpoint(RequestCheckpointRequest) returns (RequestCheckpointResponse);
```

```proto
message RequestCheckpointRequest {
  string sandbox_id = 1;
  string name = 2;
  map<string, string> labels = 3;
}
```

This is not strictly required if the first implementation maps the guest-agent
request directly into `CreateSnapshot`, but it is a better fit for the desired
"snapshot when safe" behavior.

## Backend Interfaces

Add a new optional backend capability layer on top of persistent sandboxes.
The API surface should not branch on backend name. The control plane should
gate behavior on capability flags and backend adapter support.

```go
type SnapshottingAdapter interface {
	backend.PersistentSandboxAdapter
	CreateSnapshot(context.Context, SnapshotRequest) (*SnapshotResult, error)
	ProvisionSandboxFromSnapshot(context.Context, ProvisionFromSnapshotRequest) error
	RestoreSandbox(context.Context, RestoreRequest) error
	DeleteSnapshot(context.Context, string) error
}
```

Proposed capability keys:

- `sandbox.snapshot`
- `sandbox.restore`
- `sandbox.fork`

Semantics of these capability keys:

- `sandbox.snapshot=true` means the backend can capture an immutable filesystem
  checkpoint of a sandbox
- `sandbox.restore=true` means the backend can reset a sandbox to a snapshot
- `sandbox.fork=true` means the backend can create a new sandbox from a
  snapshot

These keys describe the user-facing contract, not the implementation mechanism.
Backends may implement them with different storage drivers and different
accelerators, but the API semantics stay the same.

Initial capability rollout:

- Firecracker reports all three as `true` when snapshot support is configured
- `darwin-vz` reports all three as `false` until its later-stage implementation
  lands

Optional later accelerator capabilities can be added separately if needed, but
they should not be required for the core snapshot, restore, and fork contract.

## Firecracker Data Plane

### New volume store

Introduce a new internal volume subsystem:

- package: `internal/volumestore`
- driver interface: backend-agnostic
- first implementation: `zfs`
- later implementation: `apfs`

Responsibilities:

- seed immutable base volumes from prepared runtime rootfs images
- create writable sandbox volumes
- snapshot writable volumes
- clone snapshots into new writable volumes
- destroy volumes and snapshots
- expose a stable Firecracker drive path

The driver abstraction should be defined around operations, not filesystem
brands:

- seed base
- create writable working volume
- snapshot working volume
- clone snapshot into writable working volume
- destroy working volume
- destroy snapshot
- return the runtime attachment path for the backend

### ZFS layout

Assume a configured dataset root such as `tank/cleanroom`.

Suggested hierarchy:

- `tank/cleanroom/base/<runtime-key>`
- `tank/cleanroom/sandboxes/<sandbox-id>`
- `tank/cleanroom/sandboxes/<sandbox-id>@snap-<snapshot-id>`

Implementation choice:

- use ZFS volumes (`zvols`) as the writable block devices for Firecracker
- seed the base zvol once from the prepared ext4 runtime rootfs
- clone from base or snapshot using native ZFS clone operations

Why zvols:

- Firecracker wants a block device or raw disk image semantics
- snapshots and clones are native and cheap
- we avoid repeated large file copies and host-fs reflink assumptions

### Metadata store

ZFS tracks the data plane; Cleanroom still needs control-plane metadata.

Add a durable metadata store, preferably SQLite under XDG state:

- `XDG_STATE_HOME/cleanroom/snapshots.db`

Tables or equivalent records:

- snapshots
- sandbox volume leases
- lineage or parent pointers
- optional labels and garbage-collection state

Why not only ZFS properties:

- the control plane needs fast lookup by snapshot ID
- we need policy hash and image metadata checks
- we need snapshot listing without shelling out and parsing ZFS output on every
  control-plane request

## Darwin VZ Later-Stage Support

`darwin-vz` should adopt the same API and capability semantics later, but with a
different storage driver and an initial focus on disk-state snapshots.

### Core model

For `darwin-vz`, the portable semantic remains the same:

- snapshots capture filesystem state, not live process state
- restore and fork boot a fresh VM from the captured state
- backend-specific acceleration remains an internal detail

This keeps the control plane portable even though the runtime mechanism differs
from Firecracker.

### Storage driver

Recommended later driver:

- `apfs`

Recommended mechanism:

- keep using ext4 guest rootfs images as the guest-visible block format
- store those ext4 image files on APFS
- use APFS copy-on-write cloning for working images and snapshot images

Practical shape:

- prepared runtime rootfs image remains the seeded base artifact
- sandbox working disk is an APFS clone of that base ext4 image
- snapshot is an immutable APFS-cloned copy of the working ext4 image taken at
  a quiesced point
- fork or restore clones the snapshot image into a new writable working image

This is less sophisticated than ZFS snapshots, but it is enough to preserve the
same control-plane semantics.

### Darwin-specific lifecycle

Later `darwin-vz` snapshot flow:

1. ensure the sandbox is idle
2. ask the guest agent to quiesce
3. stop the current VM or otherwise ensure the disk image is at a safe point
4. create an APFS clone of the working ext4 image as the snapshot artifact
5. restart or continue according to the chosen implementation path

Later `darwin-vz` fork flow:

1. APFS-clone the snapshot ext4 image into a new writable working image
2. start a fresh `darwin-vz` VM from that image

Later `darwin-vz` restore flow:

1. stop the current VM
2. replace the sandbox working image with an APFS clone of the target snapshot
3. start a fresh VM for the sandbox

### Darwin-specific limits

Expected constraints:

- disk-state snapshots should be the first supported mode
- persistent sandboxes need to exist before snapshot support is useful
- host-side egress allowlist enforcement still remains a separate gap to close
- warm machine-state save and restore should be treated as a same-host
  accelerator, not the baseline primitive

### Warm checkpoint accelerator on macOS

Once disk-state support exists, `darwin-vz` can optionally add a same-host
warm-checkpoint accelerator using `Virtualization.framework` machine save and
restore.

This should remain a later optimization because:

- it is same-host scoped in practice
- compatibility is narrower than disk-state restore
- it should not change the portable API or core capability semantics

If exposed at all, it should be via optional accelerator capabilities rather
than by changing the meaning of `sandbox.snapshot`, `sandbox.restore`, or
`sandbox.fork`.

## Firecracker Lifecycle Changes

### Provision from image

Current behavior:

- prepare runtime rootfs image
- `copyFile` into `rootfs-persistent.ext4`
- launch Firecracker with that file

New behavior when snapshots are enabled:

1. ensure prepared runtime rootfs image as today
2. ensure a seeded base volume for the runtime rootfs key
3. clone base volume into `sandboxes/<sandbox-id>`
4. launch Firecracker using the cloned volume

The existing copy/reflink path can remain as the fallback when the snapshot
store is disabled.

### Create snapshot

Allowed only when the sandbox is `READY` and idle.

Flow:

1. control plane marks the sandbox busy for snapshot creation
2. if the snapshot was guest-requested, verify the request is still valid and
   the sandbox is now idle
3. control plane asks the guest agent to run the quiesce hook
4. control plane pauses the VM
5. volume store creates `@snap-<snapshot-id>` on the sandbox volume
6. control plane resumes the VM
7. metadata store records the snapshot

Important invariant:

- Cleanroom does not rely on Firecracker to manage disk snapshot files
- the host snapshot is taken only after the guest has flushed writes and the VM
  is paused

### Fork from snapshot

Flow:

1. clone snapshot into `sandboxes/<new-sandbox-id>`
2. allocate new guest CID, TAP, guest IP, and run dir
3. boot a fresh Firecracker VM from the cloned volume
4. register the new guest IP in the gateway
5. mark the sandbox `READY`

### Restore sandbox

Flow:

1. reject if the sandbox has an active execution or file download
2. terminate the current VM
3. destroy or detach the current writable volume
4. clone the target snapshot back into `sandboxes/<sandbox-id>`
5. boot a fresh VM for the same sandbox ID
6. return sandbox to `READY`

## Security and Correctness Invariants

- Snapshot create, restore, and fork are fail-closed on policy hash mismatch.
- Snapshots never contain control-plane secrets because secrets are not mounted
  into the guest filesystem by design.
- Restore and fork always produce fresh network identity.
- Snapshot creation is serialized with execution and file download operations.
- Snapshot deletion must fail while live sandboxes or child clones still depend
  on it.
- Snapshot lineage is explicit; hidden mutation of a snapshot is never allowed.

## CI-Oriented Composition Model

This primitive is useful because it composes.

Example lineage:

1. create sandbox from pinned image
2. `git clone` repository into guest
3. snapshot `src`
4. run package-manager install from lockfiles
5. snapshot `deps`
6. fork `deps` into `itest-a`, `itest-b`, and `itest-c`
7. run different integration test shards in parallel

This also supports iterative local workflows:

1. restore a sandbox to `deps`
2. rerun a destructive test
3. restore again instead of rebuilding the environment

## Runtime Config

Keep backend-neutral CLI and API, put Firecracker details in runtime config.

Proposed shape:

```yaml
backends:
  firecracker:
    snapshots:
      enabled: true
      driver: zfs
      zfs_dataset: tank/cleanroom
      quiesce_timeout_seconds: 15
  darwin-vz:
    snapshots:
      enabled: false
      driver: apfs
      base_dir: /var/tmp/cleanroom-snapshots
      quiesce_timeout_seconds: 15
```

Notes:

- snapshot support is off by default unless explicitly configured
- the first implementation only accepts `driver: zfs`
- future drivers can slot in later (`apfs`, `lvmthin`, `btrfs`, file-based COW)

## Future Distribution and Fan-Out

The execution format and the distribution format should be different concerns.

Recommended split:

- local execution format: host-native volume snapshots and clones (`zfs` in v1)
- portable distribution format: OCI artifacts stored in a registry

Why:

- ZFS snapshots are the right local primitive for fast clone and restore
- ZFS-native replication is not the right universal sharing format across large,
  heterogeneous fleets
- OCI registries already solve content addressability, authn/authz, replication,
  dedupe, and lifecycle policies

### Portable snapshot artifact

Future direction:

- export a cleanroom snapshot as an OCI artifact
- keep snapshot metadata in the manifest/config
- attach related artifacts using OCI `subject` and referrers

Recommended high-level shape:

- OCI manifest with custom `artifactType`
- config blob containing snapshot metadata:
  - schema version
  - backend family
  - `policy_hash`
  - source image digest
  - parent snapshot digest
  - filesystem payload format
  - compatibility requirements
- one or more data blobs containing the exported filesystem payload
- optional attached artifacts:
  - hotset or prefetch profile
  - provenance or SBOM
  - optional Firecracker memory checkpoint for compatible worker pools

### What to learn from the container ecosystem

#### eStargz

Useful ideas:

- lazy pulling against standard OCI registries
- workload-informed prefetching of likely startup files

Implication for Cleanroom:

- even if we do not use eStargz as the snapshot payload format, we should copy
  the "profile real workload accesses, then prefetch the hotset" idea for
  exported cleanroom snapshots

#### SOCI

Useful ideas:

- store a separate index artifact rather than mutating the original image
- preserve the original image digest and signature validity

Implication for Cleanroom:

- attach a snapshot index or hotset index as a separate OCI artifact where
  possible, instead of forcing the core snapshot artifact to change shape for
  every optimization

#### Nydus

Useful ideas:

- content-addressable filesystem format built for lazy, chunk-based access
- explicit prefetch-list support
- support for accelerating OCI images without requiring a full conversion in all
  cases

Implication for Cleanroom:

- if we need a portable snapshot payload for large-scale fan-out, a chunked,
  Nydus-like filesystem format is a stronger fit than shipping raw ext4 images
  or tar diffs
- the long-term target should be on-demand fetch of snapshot chunks, not
  mandatory whole-artifact pulls

#### Dragonfly

Useful ideas:

- peer-to-peer distribution in front of an OCI registry
- explicit preheat flows for popular artifacts
- seed-peer or proxy modes to reduce back-to-source traffic

Implication for Cleanroom:

- for fan-out to thousands of hosts, registry origin should be treated as the
  source of truth, but not as the only download path
- preheating popular golden snapshots and letting peers share chunks is likely
  far more important than shaving a few percent off local clone time

### Recommended roadmap for cross-host sharing

#### Phase A: export and import

- add `cleanroom snapshots export` and `cleanroom snapshots import`
- publish snapshots as OCI artifacts
- support pull by digest from a standard registry

Success criteria:

- a snapshot created on one host can be imported on another compatible host

#### Phase B: hotset metadata

- record file-access hotsets from bootstrap and first-run workloads
- publish hotset or prefetch metadata as an attached OCI artifact
- teach importers to prefetch likely-needed content first

Success criteria:

- imported snapshots reach useful first-command latency without requiring a full
  download of all content up front

#### Phase C: chunked portable payload

- evaluate a chunk-addressed filesystem payload for exported snapshots
- prefer a format with lazy fetch support and strong dedupe properties
- measure whether `nydus`-style distribution outperforms ext4 or tar-based
  exports materially for Cleanroom workloads

Success criteria:

- exported snapshots can be mounted or materialized incrementally, not only as
  fully downloaded monoliths

#### Phase D: large-scale fan-out

- integrate registry mirror or peer-to-peer distribution for exported snapshots
- add snapshot preheat APIs for CI orchestration
- prewarm the most common golden snapshots onto worker pools

Success criteria:

- a single snapshot publish can fan out efficiently across thousands of hosts
  without overloading the registry origin

#### Phase E: optional warm-boot accelerator

- attach Firecracker memory checkpoints as related OCI artifacts
- only enable on homogeneous worker pools with explicit compatibility checks
- keep disk snapshot artifacts as the portable baseline

Success criteria:

- compatible worker pools can resume warm checkpoints faster without changing
  the user-facing snapshot, restore, and fork semantics

## Why Firecracker VM Snapshots Are Still Useful Later

Firecracker's full snapshot support is still valuable, but as a phase-2
accelerator rather than the primary primitive.

Possible later optimization:

- capture a boot-ready Firecracker memory snapshot after guest init and
  guest-agent readiness
- on `fork`, resume from that boot checkpoint instead of cold booting
- still pair it with a cloned writable disk volume

Why this is phase 2:

- Firecracker snapshot load expects original disk and vsock paths
- resumed clones need explicit networking reconfiguration
- vsock connections are reset on restore
- cloned in-memory user-space state can duplicate uniqueness assumptions

If we add this later, the user-visible API does not change. It only makes
`fork` and `restore` faster on compatible hosts.

## Implementation Status

This section tracks what is already implemented and what remains. The current
branch lands the first functional slice with a file-backed Firecracker
implementation and a persisted snapshot metadata store. The later storage-driver
and fleet-distribution work is still pending.

### Implemented in this branch

- [done] `SnapshotService` with create, get, list, and delete
- [done] `CreateSandbox` from `snapshot_id`
- [done] `RestoreSandbox`
- [done] backend-neutral capability flags:
  - `sandbox.snapshot`
  - `sandbox.restore`
  - `sandbox.fork`
- [done] persisted snapshot metadata store in SQLite
- [done] CLI commands:
  - `cleanroom snapshot create|get|list|delete`
  - `cleanroom sandbox create --from-snapshot`
  - `cleanroom sandbox restore --snapshot`
- [done] Firecracker adapter support for snapshot, fork, and restore
- [done] control-plane concurrency checks:
  - no snapshot or restore during active exec
  - no snapshot or restore during file download
  - create-from-snapshot derives backend and policy from snapshot metadata
- [done] tests covering:
  - snapshot metadata store
  - control service snapshot lifecycle
  - Firecracker snapshot/fork/restore adapter flow

### Remaining implementation list

#### 1. Replace file-backed Firecracker storage with a volume-store abstraction

- [done] add `internal/volumestore`
- [done] define driver interface for:
  - seed base volume
  - clone writable volume
  - snapshot volume
  - destroy volume
  - destroy snapshot
- [todo] add `zfs` driver
- [done] wire Firecracker through the volume driver with a file-backed fallback
- [done] add runtime config loading for Firecracker snapshot storage
- [todo] add doctor checks for `zfs` availability and configuration

Definition of done:

- Firecracker persistent sandboxes can boot from a managed volume instead of a
  copied rootfs image
- snapshot, restore, and fork use the volume driver instead of direct file
  cloning

#### 2. Tighten Firecracker snapshot semantics

- [todo] replace host process `SIGSTOP`/`SIGCONT` with an explicit pause/resume
  or equivalent quiesce strategy if needed
- [todo] add guest-agent cooperative quiesce hook before snapshot
- [todo] add guest checkpoint request plumbing
- [todo] define behavior for pending checkpoint requests and safe-point
  materialization

Definition of done:

- snapshot creation is explicitly guest-cooperative and host-authoritative
- checkpoint requests can be issued from inside the guest and materialized once
  the sandbox is safe

#### 3. Retention and ergonomics

- [done] snapshot delete safety checks
- [done] advisory snapshot names
- [done] human-friendly CLI output
- [todo] CI examples and operational docs
- [todo] decide whether to surface snapshot lineage in CLI/API

Definition of done:

- operators can manage snapshot lifecycle without orphaning hidden storage
- the docs show the intended CI bootstrap and fan-out flow

#### 4. Add `darwin-vz` disk-state support

- [todo] add persistent `darwin-vz` sandboxes
- [todo] add `apfs` volume-store driver
- [todo] add `darwin-vz` snapshot, restore, and fork using cloned ext4 image
  files
- [todo] keep API and capability semantics identical to Firecracker

Definition of done:

- `darwin-vz` reports `sandbox.snapshot`, `sandbox.restore`, and
  `sandbox.fork=true`
- the same CLI and API calls work on both backends, with backend-specific
  runtime config

#### 5. Optional boot accelerator

- [todo] investigate a mount-namespace or jailer-based path virtualization layer
- [todo] capture boot-ready Firecracker snapshots
- [todo] resume forks from a boot checkpoint when compatible

Definition of done:

- fork latency is reduced further without changing semantics

#### 6. Portable snapshot distribution

- [todo] define OCI artifact layout for exported snapshots
- [todo] add export and import commands
- [todo] attach optional hotset or prefetch metadata
- [todo] test registry storage and digest-addressed retrieval

Definition of done:

- snapshots can move between hosts through a standard OCI registry without
  changing local execution semantics

#### 7. Fleet-scale fan-out

- [todo] evaluate chunked payload formats for exported snapshots
- [todo] integrate peer-to-peer or mirror-assisted distribution
- [todo] add preheat support for popular snapshots

Definition of done:

- widely reused snapshots can fan out across large worker fleets with low
  origin pressure and predictable startup times

## Code Touchpoints

Likely files:

- `proto/cleanroom/v1/control.proto`
- `internal/backend/backend.go`
- `internal/controlservice/service.go`
- `internal/backend/firecracker/backend.go`
- `internal/backend/darwinvz/backend_darwin.go`
- `cmd/cleanroom-darwin-vz/main.swift`
- `cmd/cleanroom-guest-agent/main.go`
- `internal/runtimeconfig/config.go`
- new `internal/volumestore/...`
- host-side control or gateway request handling for guest checkpoint requests
- CLI command wiring and tests

## External References

- OCI image artifact guidance: [OCI Image Format Spec](https://oci-playground.github.io/specs-latest/specs/image/v1.1.0-rc4/oci-image-spec.html)
- OCI referrers and subject relationships: [OCI Distribution Spec](https://oci-playground.github.io/specs-latest/specs/distribution/v1.1.0-rc2/oci-distribution-spec.html)
- OCI 1.1 overview: [Summary of Upcoming Changes in OCI Image and Distribution Specs v1.1](https://opencontainers.org/posts/blog/2023-07-07-summary-of-upcoming-changes-in-oci-image-and-distribution-specs-v-1-1/)
- eStargz lazy pull and prefetching: [stargz-snapshotter](https://github.com/containerd/stargz-snapshotter)
- SOCI index artifacts: [AWS SOCI docs](https://docs.aws.amazon.com/sagemaker/latest/dg/soci-indexing.html)
- Nydus image service and prefetch-oriented image optimization: [Nydus](https://github.com/dragonflyoss/nydus)
- Dragonfly preheat API: [Dragonfly preheat](https://d7y.io/docs/advanced-guides/open-api/preheat/)
- Dragonfly + Nydus integration: [Dragonfly Nydus integration](https://d7y.io/docs/operations/integrations/container-runtime/nydus/)

## Open Questions

- Should snapshot names be unique within a host or only advisory labels?
- Do we want snapshot TTLs or leave GC fully explicit at first?
- Should restore reject when the current sandbox policy hash differs from the
  snapshot, or do we only ever allow restore within the same sandbox lineage?

My recommendation:

- keep names advisory
- keep deletion explicit first
- require exact policy-hash match everywhere in v1
