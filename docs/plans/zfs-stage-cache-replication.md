# Firecracker ZFS Stage Cache Replication Plan

**Spec reference:** `spec.md` sections 5.1.1, 5.2, 6.4
**Status:** Active
**Last reviewed:** 2026-05-03

## Summary

Add receiver-driven stage-cache reuse between adjacent Cleanroom hosts using
ZFS incremental streams.

The first slice is deliberately narrow:

- `firecracker` backend only
- `zfs` snapshot driver only
- system-managed dependency and services stage children only
- configured peers only
- incremental transfer only when the receiver already has the parent snapshot
- no full snapshot export, APFS support, user snapshots, generic binary diff, or
  scheduler in v1
- no remote workspace-stage fill until runtime/base lineage has a dedicated
  parent contract

This keeps the hot path fast over the wire and fast to apply:

```text
sender:   zfs send -i <parent-snapshot> <child-snapshot>
receiver: zfs receive <temporary-dataset>
```

If the receiver cannot prove it has the parent lineage required by the sender,
the peer transfer is a miss and Cleanroom builds the stage locally.

## Problem

Host-local stage caches already skip repeated repository and dependency work on
one machine. A new host in the same queue or host pool still has to rebuild the
same dependency or services stage unless its local cache is warm.

Full snapshot export is too expensive for the target use case. The useful case
is usually adjacent hosts that share the lower stage already:

- both hosts have the same runtime base
- both hosts have the same workspace stage and one host wants a dependency or
  services child
- both hosts have an older dependency stage and one host wants the newer
  services stage

In those cases the transfer should be proportional to changed ZFS blocks, not
to full rootfs size.

## Goals

- Reuse Cleanroom system stage caches across adjacent hosts without rebuilding.
- Use ZFS-native incremental send/receive for the first fast path.
- Start with dependency and services children whose parent cache already exists
  locally on the receiver.
- Keep cache identity and trust decisions in the Cleanroom control plane.
- Keep driver-specific transfer complexity inside the ZFS volume driver and
  Firecracker host runtime.
- Fail closed to local rebuild when lineage, metadata, policy, backend, driver,
  or producer version do not match.
- Preserve the existing runtime model: one sealed rootfs snapshot cloned into
  one writable child per sandbox.

## Non-goals

- Transferring user snapshots.
- Cross-backend or cross-driver portability.
- macOS/APFS transfer support.
- Full rootfs artifact export.
- Generic binary diff formats.
- Dragonfly-style scheduler, gossip, P2P fanout, or multi-source piece
  fetching.
- Making system stage caches user-addressable through `--from`.
- Replacing host-local cache publication, lookup, or restore semantics.

## Current Context

The layered cache plan already separates:

- user snapshots, stored in `internal/snapshotstore`
- system stage caches, stored in `internal/cachestore`
- explicit repository changesets, stored in `internal/changesetstore`
- transport caches such as Git and OCI bytes, served through the gateway and
  content-cache paths

The current stage-cache records include:

- `stage`
- `cache_key`
- `reuse_mode`
- `state`
- `backing_snapshot_id`
- `backend`
- `policy_hash`
- `repository`
- `repository_changeset_id`
- `parent_cache_key`
- `storage_driver`
- `storage_ref`
- dependency input digests
- `created_at`
- `last_used_at`
- `producer_version`

The current volume-store interface is local-only:

```go
type Driver interface {
    Name() string
    EnsureBaseVolume(context.Context, EnsureBaseVolumeRequest) (BaseVolume, error)
    CreateWritableVolume(context.Context, CreateWritableVolumeRequest) (WritableVolume, error)
    SnapshotVolume(context.Context, SnapshotVolumeRequest) (Snapshot, error)
    CloneSnapshotToVolume(context.Context, CloneSnapshotToVolumeRequest) (WritableVolume, error)
    DestroyVolume(context.Context, DestroyVolumeRequest) error
    DestroySnapshot(context.Context, DestroySnapshotRequest) error
}
```

This plan adds an optional transfer capability beside the local volume-store
operations. Local clone/restore remains the normal execution path after an
import succeeds.

## Current Progress

The foundation slice has landed:

- runtime `cache.peers` config shape
- cache record lineage fields for architecture, runtime base, sizing, driver
  metadata, import provenance, and validation time
- ZFS snapshot description metadata with snapshot GUID and parent GUID capture
- helper-gated ZFS metadata reads for mixed-version root-helper deployments
- incremental export planning with `zfs send -nP -i`

The local ZFS transfer primitive has landed:

- stream an already validated incremental export with `zfs send -i`
- receive a stream into an unpublished managed ZFS dataset cloned from the
  local parent snapshot
- destroy the unpublished dataset on receive or validation failure
- prove the clone, receive, promote, and describe contract with a gated real
  ZFS integration test
- inventory and prune unreferenced import datasets through `system df`,
  `system prune`, and daemon startup cleanup

The sender side of the peer protocol has landed:

- lookup RPC and export HTTP endpoint
- sender-side transfer tokens
- dependency and services child exports only, with an explicit parent stage,
  parent cache key, and parent ZFS GUID

The next slice is receiver-side import orchestration:

- receiver-side import orchestration around the proven local ZFS primitive
- cache publication only after receiver-side metadata validation
- dependency/services import attempts only after the receiver already has the
  workspace or dependency parent locally

## Design Principles

### 1. Incremental transfer is a cache fill, not a sandbox source

Peer transfer populates local `cachestore` and local ZFS snapshot storage. Once
imported, sandbox creation uses the existing local cache restore path.

### 2. The receiver owns trust

The sender can advertise and stream a candidate, but the receiver must
revalidate before marking the imported record `ready`.

Receiver validation includes:

- backend is `firecracker`
- storage driver is `zfs`
- architecture and runtime base match
- producer version is compatible
- requested `stage/cache_key` matches local derivation
- policy hash matches the local compiled policy
- parent cache key and parent ZFS GUID match the receiver's local parent
- repository metadata and changeset metadata match the expected stage
- dependency or services key-file digests match the expected stage, where
  applicable

### 3. Common parent lineage is required

The v1 transfer path only runs when both hosts can prove a common parent
snapshot by ZFS GUID.

The receiver sends the parent GUID it already has. The sender may export only
if its candidate child is incrementally sendable from a local snapshot with the
same parent GUID.

If the common parent is missing, ambiguous, or not sendable, the result is a
miss.

### 4. Temporary imports are not visible

Imported ZFS datasets and cache metadata stay temporary until validation
passes. Failed imports must destroy temporary datasets and leave no `ready`
record behind.

### 5. Static peers before schedulers

Start with configured peers in runtime config. Receiver-side parallel lookup is
enough for the first host-pool deployment. A scheduler or Dragonfly-style fanout
can be added later if static peers become the bottleneck.

## Runtime Configuration

Add a cache peer section to runtime config:

```yaml
cache:
  peers:
    - url: https://cleanroom-a.internal:8989
      token_env: CLEANROOM_CACHE_PEER_TOKEN
    - url: https://cleanroom-b.internal:8989
      token_env: CLEANROOM_CACHE_PEER_TOKEN
```

Rules:

- absent `cache.peers` means peer import is disabled
- peer URLs are daemon control-plane URLs
- v1 authentication can use bearer tokens loaded from `token_env`
- mTLS can be added later without changing the transfer model
- peers are tried only for `firecracker` + `zfs` stage caches

## Cache Metadata Additions

Extend stage-cache metadata with driver-specific lineage fields. Keep them
generic at the store boundary, with ZFS-specific values carried in structured
driver metadata.

Suggested additions:

```text
architecture
runtime_base_key
storage_size_bytes
exclusive_size_bytes
driver_metadata_proto/json
imported_from_peer
last_validated_at
```

Suggested ZFS driver metadata:

```text
zfs_dataset
zfs_snapshot
zfs_snapshot_guid
zfs_parent_snapshot_guid
zfs_receive_stream_version
```

The first implementation can store driver metadata as a versioned JSON blob in
`cachestore` if that keeps schema churn smaller. The JSON shape must be stable
and covered by tests because it becomes part of peer compatibility.

## Peer API

Use Connect RPC for metadata and normal request/response APIs. Serve the raw
ZFS send stream over HTTP so it can be piped directly into `zfs receive` without
protobuf framing.

### Lookup

Request:

```text
stage
cache_key
backend
storage_driver
architecture
producer_version
policy_hash
parent_stage
parent_cache_key
parent_zfs_snapshot_guid
```

Response:

```text
miss
```

or:

```text
candidate:
  transfer_token
  stage
  cache_key
  backing_snapshot_id
  storage_ref
  parent_stage
  parent_cache_key
  zfs_snapshot_guid
  zfs_parent_snapshot_guid
  producer_version
  backend
  storage_driver
  architecture
  policy_hash
  estimated_bytes
  expires_at
```

Rules:

- only `ready` records can be advertised
- the sender must verify the requested parent GUID matches the candidate
  lineage before issuing a transfer token
- transfer tokens are short-lived and single-use
- tokens bind the exact request fields, candidate cache record, and parent GUID

### Export

Endpoint:

```text
GET /v1/cache/export/zfs-incremental/<transfer-token>
```

Response:

```text
application/octet-stream
```

The sender runs the ZFS driver export operation and streams stdout directly to
the HTTP response.

### Import

Import is receiver-local. The receiver calls its ZFS driver with:

```text
transfer_token metadata
local parent storage ref
local parent ZFS GUID
incoming zfs stream reader
temporary snapshot id
```

The driver receives into a temporary dataset and returns a local `storage_ref`
and ZFS metadata only after the stream is fully applied.

## ZFS Driver Capability

Add an optional interface beside `volumestore.Driver`:

```go
type IncrementalSnapshotTransferDriver interface {
    DescribeSnapshot(context.Context, DescribeSnapshotRequest) (SnapshotDescription, error)
    PlanIncrementalSnapshotExport(context.Context, IncrementalSnapshotExportRequest) (IncrementalSnapshotExportPlan, error)
    ExportIncrementalSnapshot(context.Context, IncrementalSnapshotExportPlan, io.Writer) error
    ImportIncrementalSnapshot(context.Context, IncrementalSnapshotImportRequest, io.Reader) (Snapshot, error)
}
```

Suggested request and response shape:

```go
type SnapshotDescription struct {
    Driver              string
    StorageRef          string
    SnapshotGUID        string
    ParentSnapshotGUID  string
    SizeBytes           int64
    ExclusiveSizeBytes  int64
    DriverMetadata      map[string]string
}

type IncrementalTransferPlan struct {
    FromStorageRef       string
    FromSnapshotGUID     string
    ToStorageRef         string
    ToSnapshotGUID       string
    EstimatedBytes       int64
    DriverMetadata       map[string]string
}
```

The exact command sequence belongs inside the ZFS driver or Firecracker host
runtime. The first implementation must prove with a real ZFS test that the
current stage snapshot creation flow preserves a sendable parent relationship.
If `SnapshotVolume` promotion breaks the desired incremental lineage, update
the ZFS driver to preserve sendable lineage or create a stable bookmark during
publication.

## Receiver Flow

When stage-cache lookup misses locally:

1. Derive the desired stage cache key exactly as today.
2. Identify the logical parent stage cache key.
3. Load the local parent cache record.
4. Describe the local parent ZFS snapshot and read its GUID.
5. Query configured peers in parallel with the desired key and parent GUID.
6. Pick the first compatible candidate.
7. Reserve a local import slot for `stage/cache_key` so duplicate imports
   coalesce.
8. Open the peer export endpoint.
9. Pipe the response body into local `ImportIncremental`.
10. Validate the imported record against local request inputs.
11. Insert or upsert a `ready` `cachestore` record with the local storage ref.
12. Use the existing local restore path to create the sandbox.

If any step fails, destroy the temporary import and continue to the next peer or
fall back to local build.

## Sender Flow

When a peer lookup arrives:

1. Authenticate the peer request.
2. Load the requested `stage/cache_key`.
3. Reject non-ready records.
4. Reject non-`firecracker` or non-`zfs` records.
5. Compare backend, driver, architecture, producer version, policy hash,
   parent stage, parent cache key, and parent ZFS GUID.
6. Ask the ZFS driver for an incremental export plan.
7. Mint a short-lived transfer token bound to the candidate and request.

When export starts:

1. Revalidate the token and candidate record.
2. Start the ZFS incremental send.
3. Stream bytes to the response.
4. Record bytes sent, duration, result, and error if any.

## Import Publication Rules

Imported records must follow the same safety rules as locally published stage
caches:

- metadata becomes visible only after storage import succeeds
- only `ready` entries are reusable
- failed imports do not overwrite existing ready entries
- ready entries are immutable
- replaced records must delete old storage only after the replacement metadata
  is durable
- imported records must use the receiver's local storage ref, not the sender's
  storage ref

## Cleanup and Retention

Peer import makes ref-safe cleanup more important.

Before adding broad retention policy, add the minimum safe cleanup model:

- cache records refer to storage refs and backing snapshot ids
- multiple records may share one storage ref
- delete storage only after no `ready` record references it
- in-flight exports and imports pin their source and destination refs
- temporary receive datasets are destroyed on failure and on daemon startup
  cleanup
- `system df` and `system prune` include Firecracker/ZFS import datasets when
  the host is configured for the ZFS snapshot driver
- daemon startup queues import cleanup onto the same bounded background worker
  used for terminated sandbox storage cleanup
- cleanup only destroys direct children under `snapshots/imports/<id>` that are
  not referenced by snapshot or cache metadata

Full quota-based eviction can follow this plan, but v1 must not make shared
storage refs unsafe.

## Observability

Add structured logs, traces, and metrics for:

- peer lookup count, hit, miss, and error
- chosen peer
- parent GUID match or mismatch
- export bytes and duration
- import bytes and duration
- import validation result
- fallback reason
- local rebuild after peer miss

Suggested metric names:

```text
cleanroom_cache_peer_lookup_total{stage,result}
cleanroom_cache_peer_transfer_bytes_total{stage,direction}
cleanroom_cache_peer_transfer_duration_seconds{stage,result}
cleanroom_cache_peer_import_total{stage,result}
```

Do not put raw cache keys, snapshot IDs, storage refs, or peer tokens in metrics
labels.

## Security

The peer API exposes powerful local cache data. Keep v1 conservative:

- require authentication for lookup and export
- accept v1 bearer tokens only from configured `cache.peers[*].token_env`
- restrict exports to system stage caches
- never export user snapshots
- never export non-ready records
- bind transfer tokens to exact lookup inputs
- expire transfer tokens quickly
- avoid logging bearer tokens or transfer tokens
- rate-limit concurrent exports per peer
- validate imported metadata locally before publishing

## Code Touchpoints

| File | Change |
|---|---|
| `internal/runtimeconfig/config.go` | Add `CacheConfig` with configured peers. |
| `proto/cleanroom/v1/control.proto` | Add peer cache lookup RPC messages or a new peer cache service. |
| `internal/controlservice/service.go` | Hook peer import into stage-cache miss handling before local build. |
| `internal/controlservice/workspace_stage.go` | Keep workspace-stage peer import out of the v1 slice until parent runtime/base lineage has a dedicated contract. |
| `internal/controlservice/dependency_stage.go` | Include peer import for exact dependency and portable dependency records where parent lineage can be proven. |
| `internal/controlservice/services_stage.go` | Include peer import for services-stage misses where parent dependency/workspace lineage can be proven. |
| `internal/cachestore/store.go` | Add driver lineage metadata, architecture/runtime fields, and storage-size fields. |
| `internal/volumestore/store.go` | Add optional incremental snapshot transfer interface. |
| `internal/volumestore/zfs.go` | Implement ZFS description, incremental export, and incremental import. |
| `internal/backend/firecracker/host_runtime.go` | Add helper-backed ZFS send/receive operations if direct `zfs` access stays behind the privileged helper. |
| `internal/storagegc/storagegc.go` | Inventory and prune unreferenced ZFS import datasets without touching referenced cache storage. |
| `internal/cli/system.go` | Include ZFS import datasets in `system df` and `system prune` on Firecracker/ZFS hosts. |
| `scripts/cleanroom-root-helper.sh` | Allow narrowly scoped `zfs get guid`, `zfs send`, `zfs receive`, and import-namespace `zfs list` operations for managed Cleanroom refs only. |
| `internal/observability/*` | Add peer cache transfer span attributes and metrics. |

## Implementation Order

### 1. Metadata and local description

Status: landed.

- Add cache metadata fields needed to describe ZFS lineage.
- Teach ZFS stage publication to record snapshot GUID and parent GUID.
- Add tests that published workspace, dependency, and services records carry
  ZFS lineage metadata when the storage driver is `zfs`.

### 2. ZFS incremental transfer contract

Status: landed.

- Add the optional transfer interface in `internal/volumestore`.
- Implement `DescribeSnapshot` for ZFS using ZFS GUID properties.
- Implement incremental export planning with strict parent GUID checks.
- Stream validated exports with `zfs send -i`.
- Add real or helper-backed ZFS tests proving the current snapshot lineage can
  produce an incremental stream.

If this test fails because current promotion changes lineage in a way that
prevents incremental send, fix the ZFS driver before adding peer APIs.

### 3. Import into temporary storage

Status: landed.

- Implement ZFS incremental receive into an unpublished managed dataset cloned
  from the local parent snapshot.
- Return a local `Snapshot` only after receive succeeds.
- Destroy temp datasets on failure.
- Add daemon startup cleanup for stale temporary import datasets.

### 4. Static peer lookup API

Status: sender side landed; receiver client is next.

- Add runtime config for peer URLs and token env vars.
- Add lookup RPC and export HTTP endpoint.
- Add authentication and short-lived transfer tokens.
- Add sender-side tests for miss, incompatible parent, incompatible backend,
  non-ready record, and successful candidate.

### 5. Receiver-side peer import

Status: next.

- On local stage-cache miss, query configured peers when parent ZFS lineage is
  available.
- Attempt v1 peer import only for dependency and services stage children, not
  workspace-stage misses.
- Coalesce duplicate imports for the same `stage/cache_key`.
- Stream export response directly into `ImportIncremental`.
- Validate imported metadata locally.
- Publish the imported cache record and use the existing restore path.

### 6. Observability and fallback hardening

- Add logs, traces, and metrics.
- Ensure every peer miss or failed import falls back to local build.
- Ensure failed import leaves no ready metadata and no temporary ZFS dataset.

### 7. End-to-end validation

- Add a two-daemon ZFS integration test gated behind an environment flag.
- Build a parent stage on both hosts, build a child stage on one host, then
  prove the second host imports the child through incremental send/receive.
- Verify the imported stage restores normally and skips the local bootstrap
  work it would otherwise have run.

## Testing Plan

### Unit tests

- cache metadata round trips driver lineage fields
- peer lookup rejects missing, non-ready, backend mismatch, driver mismatch,
  policy mismatch, producer mismatch, and parent GUID mismatch
- transfer token binds exact request fields and expires
- duplicate imports coalesce
- import failure does not publish metadata

### ZFS driver tests

- describe snapshot returns stable GUID metadata
- parent GUID mismatch refuses export
- export streams only after lineage validation
- import writes into a temporary dataset
- failed receive destroys temporary dataset
- successful receive returns a local storage ref and GUID metadata

### Integration tests

- local parent plus peer child imports successfully
- no local parent falls back to build
- stale peer metadata falls back to build
- interrupted transfer falls back to build and cleans temp storage
- imported dependency stage restores and skips dependency bootstrap
- imported services stage restores and skips services bootstrap

## Rollout

1. Ship disabled by default.
2. Enable only on Firecracker/ZFS hosts in one queue or host pool.
3. Start with one or two configured peers per host.
4. Measure peer hit rate, transferred bytes, import duration, and fallback rate.
5. Expand peer lists only after import validation and cleanup behavior are
   stable.

## Future Work

- Workspace-stage peer import once runtime/base lineage has a dedicated parent
  identity and validation contract.
- Scheduler or peer registry for larger pools.
- Dragonfly-style piece scheduling and fanout if one sender becomes a
  bottleneck.
- Full snapshot or chunked artifact fallback when no common parent exists.
- APFS parent-clone plus block-delta import.
- Quota-based GC and retention policy.
- Offline warm-cache mode that can require peer import or fail closed.

## Open Questions

- Should peer lookup happen before or after portable dependency-stage lookup?
- Should imported records preserve the sender's `created_at`, or should they
  use receiver import time with sender provenance in driver metadata?
- Do we need separate inbound and outbound peer auth config before exposing
  this outside one trusted host pool?
- Should peer transfer be opportunistic only, or should policies later be able
  to require remote cache import before rebuilding?
