# Snapshot Plan

**Status:** In progress
**Scope:** file-backed Firecracker and `darwin-vz` first; native COW drivers later

## Summary

Add one new environment primitive:

- `snapshot`: capture an immutable cleanroom filesystem state.

A snapshot can then be used as the source for:

- `sandbox create --from <snapshot-id>`
- `exec --from <snapshot-id>`
- `console --from <snapshot-id>`

The user-visible contract is intentionally filesystem-centric, not live-VM-centric.
Creating from a snapshot always results in a fresh VM boot from the captured
state. That keeps the primitive deterministic, safe to compose, and compatible
with Cleanroom's existing network identity and guest-agent model.

The mergeable first slice uses a `file` driver to establish the API, metadata,
and lifecycle semantics on both Firecracker and `darwin-vz`. Follow-ups add
native COW drivers (`zfs` for Firecracker and `apfs` for `darwin-vz`) so
snapshot-backed sandboxes become host-native clones instead of copied ext4
files.

This gives us a strong base for CI:

1. start from a deterministic image-backed cleanroom
2. run bootstrap work once (`git clone`, dependency install, generated fixtures)
3. snapshot the result
4. create many isolated sandboxes from that golden state

## Why Fresh-Boot Snapshots

The tempting alternative is to expose Firecracker's full VM snapshot/restore as
the primary primitive. We should not do that.

Reasons:

- Cleanroom cares about reusable environment state, not duplicated process state.
- Fresh boot gives every snapshot-backed sandbox a new sandbox ID, guest IP,
  gateway registration, and vsock endpoint.
- We avoid cloning open TCP connections, old vsock sessions, wall-clock drift,
  and any in-memory uniqueness bugs from resumed workloads.
- Firecracker snapshot load requires the original disk/vsock resources to appear
  at the same paths, which complicates concurrent reuse materially.
- Firecracker's own snapshot docs call out clone networking, vsock reset, and
  uniqueness concerns; those are acceptable as an optimization layer, but they
  are the wrong default semantics for CI primitives.

The result is a simpler contract:

- `snapshot` captures disk state only
- `create --from`, `exec --from`, and `console --from` always boot a new VM
  from that disk state
- any later Firecracker resume fast-path is an internal optimization, not a
  change in user-facing semantics

## Goals

- Capture a golden cleanroom state after deterministic setup work.
- Make snapshot-backed sandbox creation near-constant-time at the storage layer.
- Keep the CLI, API, and core capability surface backend-neutral even if only
  Firecracker implements it first.
- Preserve existing policy immutability and gateway security invariants.
- Allow snapshot lineage: a sandbox created from a snapshot can create later
  snapshots.
- Keep snapshots durable across daemon restarts.
- Allow the guest workload to mark checkpoint-safe moments cooperatively.

## Non-Goals

- Exposing live process or in-memory checkpoint semantics to users.
- Cross-backend snapshot portability.
- Reusing a snapshot under a different compiled policy hash.
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
- `name` (optional)
- storage reference (`volume_snapshot_ref`)
- repository checkout metadata when present

Semantics:

- Captures the writable root volume only.
- Does not capture running processes, sockets, network identity, or active execs.
- Can be used as the parent for many new sandboxes.
- Can be the parent of later snapshots, creating a lineage graph.

### Create From Snapshot

Provision a new sandbox from a snapshot.

Semantics:

- New sandbox ID.
- New guest IP / TAP / gateway registration.
- New writable child volume cloned from the snapshot.
- Fresh VM boot.
- Same compiled policy and backend lineage as the snapshot.
- Preserves sandbox metadata that is part of environment construction, such as
  repository checkout state.

There is no separate `fork` primitive. Creating from a snapshot is the only
fan-out operation the user needs.

There is no separate `restore` primitive. Rewinding an environment is modeled as
creating a fresh sandbox from the earlier snapshot instead of mutating an
existing sandbox in place.

## API Surface

Keep the control plane backend-neutral. Backend-specific storage details live in
runtime config and adapter internals.

### Snapshot service

```proto
service SnapshotService {
  rpc CreateSnapshot(CreateSnapshotRequest) returns (CreateSnapshotResponse);
  rpc GetSnapshot(GetSnapshotRequest) returns (GetSnapshotResponse);
  rpc ListSnapshots(ListSnapshotsRequest) returns (ListSnapshotsResponse);
  rpc DeleteSnapshot(DeleteSnapshotRequest) returns (DeleteSnapshotResponse);
}
```

### Sandbox creation from snapshot

`CreateSandboxRequest` supports a snapshot source:

```proto
message CreateSandboxRequest {
  string backend = 2;
  SandboxOptions options = 3;
  Policy policy = 4;
  RepositoryCheckout repository_checkout = 5;

  oneof source {
    string snapshot_id = 6;
  }
}
```

Rules:

- when `snapshot_id` is set, `policy` must be omitted
- when `snapshot_id` is set, `repository_checkout` must be omitted
- backend and policy are derived from the snapshot metadata
- any mismatch fails closed

### CLI shape

Commands:

- `cleanroom snapshot create <sandbox-id> [--name <name>]`
- `cleanroom snapshot inspect <snapshot-id> [--json]`
- `cleanroom snapshot ls`
- `cleanroom snapshot rm <snapshot-id>`
- `cleanroom sandbox create --from <snapshot-id>`
- `cleanroom exec --from <snapshot-id> -- <command>`
- `cleanroom console --from <snapshot-id>`

Lifecycle rules:

- `exec` and `console` are ephemeral by default when they create a sandbox.
- `--keep` preserves a newly created sandbox for later reuse.
- `--in <sandbox-id>` reuses an existing sandbox and never infers repository
  state from the caller's current working directory.
- `--from <snapshot-id>` creates a new sandbox from a snapshot and runs inside
  it.

## Guest-Cooperative Checkpoint Trigger

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

Recommended flow:

1. workload inside the guest invokes `cleanroom-guest-agent checkpoint request`
2. guest agent sends a checkpoint request to the host control plane
3. control plane records the request against the sandbox, with optional
   metadata such as name and labels
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

## Capability Surface

The portable capability surface is intentionally small:

- `sandbox.snapshot`
- `sandbox.persistent`

Meaning:

- `sandbox.snapshot=true` means the backend can create snapshots and create new
  sandboxes from those snapshots
- `sandbox.persistent=true` means the backend supports reusable sandbox IDs

There are no dedicated `sandbox.restore` or `sandbox.fork` capabilities.

## Backend Notes

### Firecracker

The first implementation is file-backed:

- snapshot creates an immutable copy of the writable ext4 rootfs
- create-from-snapshot provisions a new writable rootfs from that copy
- delete removes the stored snapshot rootfs

Follow-up work switches the volume store to ZFS clones so snapshot-backed
sandbox creation becomes cheap at the storage layer without changing the user
contract.

### Darwin VZ

The first implementation is also file-backed:

- snapshot pauses the VM, syncs the guest, and copies the writable ext4 rootfs
- create-from-snapshot provisions a new persistent sandbox from that stored
  image
- delete removes the stored snapshot rootfs

Follow-up work can switch the volume store to APFS cloning and optionally add
same-host machine-state acceleration, again without changing the user contract.

## Policy and Identity Rules

- Snapshot creation and create-from-snapshot are fail-closed on policy hash
  mismatch.
- Snapshot-backed sandboxes always receive fresh network identity.
- Snapshot metadata persists enough sandbox metadata to recreate expected
  working-directory behavior, including repository checkout state.

## Example CI Flow

1. create a sandbox
2. bootstrap `/workspace` and dependencies
3. run `cleanroom snapshot create`
4. run `cleanroom exec --from <snapshot-id> -- make test-a`
5. run `cleanroom exec --from <snapshot-id> -- make test-b`
6. run `cleanroom exec --from <snapshot-id> -- make test-c`

## Future Distribution and Fan-Out

Execution format and distribution format should stay separate.

Local execution format:

- native host volume store (`file` first, then `zfs` / `apfs`)

Portable distribution format:

- OCI artifact carrying snapshot metadata plus filesystem payload
- optional attached hotset/prefetch metadata
- optional attached machine-state checkpoint for homogeneous pools only

Promising future directions:

- Nydus-style chunk-addressed payloads for lazy, deduplicated delivery
- Dragonfly-style mirror + P2P fan-out for large fleets
- Stargz/SOCI-style hotset metadata for startup prefetch

These should accelerate snapshot distribution without changing the local
snapshot semantics.

## Implementation List

- [done] snapshot metadata store
- [done] `SnapshotService`
- [done] `CreateSandbox` support for `snapshot_id`
- [done] CLI commands for `snapshot create|inspect|ls|rm`
- [done] `cleanroom sandbox create --from`
- [done] `cleanroom exec --from`
- [done] `cleanroom console --from`
- [done] snapshot metadata preserves repository checkout state
- [done] file-backed Firecracker adapter support
- [done] file-backed `darwin-vz` adapter support
- [done] snapshot inspect output with stored metadata
- [todo] guest-cooperative checkpoint request flow
- [todo] ZFS volume store for Firecracker
- [done] APFS volume store for `darwin-vz`
- [todo] OCI export/import for cross-host distribution
- [todo] chunked/lazy fleet fan-out
- [todo] optional same-host machine-state acceleration

## External References

- Firecracker snapshot support docs
- Firecracker network clone docs
- OCI artifact and referrers specs
- stargz-snapshotter
- AWS SOCI
- Nydus
- Dragonfly
