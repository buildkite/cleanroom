# Layered Cache Plan

**Spec reference:** `spec.md` sections 5.1.1, 5.2, 6.4
**Status:** In progress
**Last reviewed:** 2026-04-11

## Summary

Build a deterministic, host-local cache pipeline for environment construction,
then
materialize the selected cached environment as a single writable root volume
per sandbox.

This plan intentionally keeps two systems separate:

- user snapshots are explicit, user-facing savepoints
- system caches are implicit, host-managed stage outputs

The system-cache pipeline uses stage terminology:

- runtime stage: prepared runtime rootfs
- workspace stage: runtime rootfs plus exact repository checkout
- dependency stage: workspace stage plus deterministic bootstrap/dependencies

Each stage output is a full sealed rootfs snapshot. Higher stages subsume lower
stages. At runtime we do not mount `runtime + workspace + dependency` as a live
stack. We clone the best ready stage into one writable child and boot that as
the only guest root disk.

The core system-cache model is:

- host-side caches are keyed by immutable inputs such as image digests, git
  commits, lockfiles, toolchain manifests, and compiled policy hashes
- cached environments are published only by trusted host-side flows after
  verification
- sandboxes do not mount a live stack of cache layers at runtime
- sandboxes boot from one writable child volume cloned from the best sealed
  cached snapshot

This separates two concerns that are easy to conflate:

- **cache design** decides what reusable artifacts exist and how they are keyed
- **materialization design** decides how a selected artifact becomes a fast VM
  root filesystem

We need both. Deterministic caches without cheap clone materialization still
leave startup expensive. Cheap block clones without deterministic cache keys
still leave reuse unsafe and hard to reason about.

## Terminology

This document uses:

- **stage** for the transformation step in the cache pipeline
- **cache entry** for the immutable stored output of a stage

Current implementation still uses some "seed" naming for stage outputs,
especially workspace-stage snapshots. This document uses stage terminology
going forward because it maps more clearly to how the cache pipeline is built.

## Delivery Strategy

This plan should land in phases rather than as one large cross-backend change.

### Phase 1: Workspace-stage orchestration

Status: largely landed for snapshot-capable backends. Current implementation
names this the workspace-seed flow.

The backend-neutral control-plane slice is now in place:

- publish workspace-stage snapshots after exact-commit repository bootstrap
- look up matching workspace-stage snapshots before repeating bootstrap
- restore matching workspace-stage snapshots before cold bootstrap
- republish after runtime-base changes or restore failure fallbacks

This phase improves warm repo-aware sandbox creation by skipping repeated
checkout work on snapshot-capable persistent backends.

Trusted publication semantics are present in a narrow form: the control plane
creates the snapshot, persists metadata only after snapshot creation succeeds,
and rolls the snapshot back if metadata persistence fails.

### Phase 2: Firecracker hot-path materialization

Status: partially landed.

The Firecracker runtime plumbing now exists:

- route normal execution through writable root volume preparation
- consume published stage caches through the volume-store clone path
- allow clone-capable drivers such as ZFS to materialize writable child volumes

The remaining gap is deployment/runtime-dependent rather than architectural:
the default Firecracker `file` driver still copies ext4 bytes, so the "nearly
plain sandbox boot time" win only happens on clone-capable storage such as ZFS.

### Phase 3: Dedicated cache store and dependency stage

Status: not started.

The next simplification step should be:

- add a dedicated `cachestore` for system-managed stage outputs
- keep `snapshotstore` as the user snapshot store
- move workspace-stage metadata out of reserved snapshot naming
- add one strict dependency stage for a single ecosystem first
- keep distribution/export out of scope until the host-local model is proven

## Current Progress Snapshot

### Landed

- host-side git mirror transport cache keyed by canonical remote URL
- exact-commit repository bootstrap through the host-controlled flow
- runtime-base-key derivation for `firecracker` and `darwin-vz`
- workspace-stage publish, lookup, restore, and fallback republish in the
  control service
- one writable-root-volume preparation path for Firecracker normal execution
  and snapshot restore

### Partial

- Firecracker hot-path materialization is wired through the volume-store path,
  but clone-based behavior still depends on the configured storage driver

### Not started

- dedicated `cachestore` for system-managed stage outputs
- dependency-stage caches
- lockfile-keyed dependency stage publication for any ecosystem
- strict offline warm-cache mode
- garbage collection and retention policy
- cross-host distribution/export for stage caches

### Current caveats

- workspace-stage identity is currently stored as a managed snapshot name, not
  a first-class cache entry record
- workspace-stage keying currently includes the local checkout branch because
  repository bootstrap can create either a detached checkout or a named local
  branch

## Background

The repository already has most of the right primitives in separate slices:

- repo-aware bootstrap resolves the current repository to an exact remote URL
  and commit and already plans for a mirror-backed git gateway
- snapshots are modeled as immutable filesystem artifacts that can fan out to
  many fresh-boot sandboxes
- Firecracker and `darwin-vz` both already understand "one writable root volume
  attached to the VM"
- the volume store already exposes `EnsureBaseVolume`,
  `CreateWritableVolume`, `SnapshotVolume`, and `CloneSnapshotToVolume`

What is still missing is a dedicated system-cache model that answers:

- which stage outputs should exist
- which exact inputs produce them
- who is allowed to publish them
- when a cache hit is trusted
- how warm hits become close to plain sandbox creation time

The important simplification is to avoid over-unifying this with user
snapshots. User snapshots and system caches can share low-level storage
mechanisms, but they should remain separate top-level features with different
identity, lifecycle, and API semantics.

## Goals

- Make warm environment startup close to plain sandbox boot time.
- Key reusable environments by immutable, replayable inputs.
- Keep credentials and upstream auth on the host side.
- Prevent cache poisoning by untrusted guest workloads.
- Support offline warm-cache execution with fail-closed behavior.
- Keep the guest runtime model backend-neutral: one writable root filesystem
  per sandbox, fresh boot every time.

## Non-goals

- Exposing runtime union filesystem semantics to users.
- Reusing mutable package-manager caches directly from untrusted sandboxes.
- Treating moving refs such as branches or tags as reusable cache keys.
- Solving every ecosystem's lockfile format in the first slice.
- Unifying user snapshots and system caches into one metadata/API surface.
- Guaranteeing zero supply-chain risk in an absolute sense.

## Design Principles

### 1. Keep user snapshots and system caches separate

User snapshots and system caches may use the same low-level clone/snapshot
mechanisms, but they are different products:

- user snapshots are addressed by snapshot ID and user intent
- system caches are addressed by `(stage, key)` and deterministic inputs
- user snapshots are explicit and durable
- system caches are implicit and eligible for automatic garbage collection

`snapshotstore` should remain the user snapshot store. System-managed stage
outputs should move into a dedicated `cachestore`.

### 2. Keys must be derived from immutable inputs

Reusable stage caches must never be keyed by:

- image tags
- branch names
- plain repository URLs without a commit
- registry URLs without an integrity hash
- mutable host-local cache state

They must instead be keyed by exact inputs such as:

- OCI digest
- canonical remote URL
- full commit SHA
- submodule SHAs or enabled/disabled state
- lockfile bytes and lockfile parser version
- toolchain manifest bytes and resolved tool versions
- compiled policy hash
- bootstrap recipe digest

### 3. System caches are stage outputs

The cache pipeline should be described as ordered stages:

- runtime
- workspace
- dependency

Each stage produces one immutable cached output. Higher stages subsume the
lower stages logically, even if the runtime only boots from one concrete rootfs
snapshot.

### 4. Each system cache entry is a full rootfs snapshot

We should not assemble a live guest-visible mount stack like "base + workspace
+ dependency" for the default path. Each stage cache entry should be one sealed
rootfs snapshot that already contains the lower stages' contents.

That means:

- `/workspace` stays part of the main rootfs, not its own default volume
- boot always attaches one writable child root disk
- runtime layering exists in metadata and lineage, not in guest mounts

### 5. Untrusted sandboxes must not publish shared cache entries

A guest workload can request a checkpoint or complete a bootstrap step, but the
host control plane remains authoritative for promotion into a shared cache.

That means:

- build into a temporary sandbox or temporary snapshot
- verify the result on the host side
- publish atomically
- never mutate a `ready` cache entry in place

### 6. Separate transport caches from environment caches

A git mirror or a package artifact CAS improves fetch cost and upstream load,
but it is not itself a runnable environment.

That distinction matters for correctness:

- transport caches are host-side accelerators
- environment caches are sealed rootfs states that can be cloned into sandboxes

### 7. Keep the API/backend model simple and host-local first

The first slice should stay backend-neutral at the API surface and host-local
in storage behavior:

- keep stage keys and stage selection backend-agnostic
- keep storage-driver differences inside runtime config and adapter internals
- do not design cross-host distribution into the local metadata model yet

## Cache Types

We should model three distinct things explicitly.

### A. System stage caches

This is the host-managed environment cache pipeline:

- runtime stage cache
- workspace stage cache
- dependency stage cache

These are reusable rootfs states that can be cloned into disposable sandboxes.

### B. Transport caches

These reduce upstream fetch cost but are not runnable environments:

- git mirror transport cache
- gateway git response cache
- gateway OCI/blob cache

The existing git mirror is already in-tree. The gateway Git/OCI content-cache
work proposed in PR #138 fits here as a transport-cache plane, not as a system
stage cache.

### C. Runtime materialization

This is how a selected stage cache becomes one writable VM root disk:

- plain file copy
- APFS `clonefile`
- ZFS snapshot/clone

Stage cache selection is backend-neutral. Materialization remains
backend-specific.

## System Cache Stages

### Stage 0: Runtime

Purpose:

- derive the prepared runtime rootfs from the pinned OCI image digest
- inject guest runtime assets once

Suggested key:

```text
runtime_base_key = H(
  schema_version,
  backend_family,
  architecture,
  image_digest,
  guest_runtime_version,
  kernel_asset_id,
  rootfs_prepare_recipe_digest
)
```

Output:

- immutable prepared runtime rootfs stage cache
- optionally stored as a managed base volume

Notes:

- this is not repository-specific
- this is the first layer that should be reused aggressively across runs

### Stage 1: Workspace

Purpose:

- capture an exact checked-out repository state ready for command execution

Suggested key:

```text
workspace_stage_key = H(
  runtime_base_key,
  canonical_remote_url,
  commit_sha,
  submodule_mode,
  submodule_resolution_digest,
  checkout_mode,
  repository_destination_dir
)
```

Output:

- sealed rootfs snapshot containing the exact repository checkout

Notes:

- this is the next speed stage after mirror-backed clone
- it turns "avoid hammering upstream" into "skip clone and checkout on warm hit"
- current implementation calls this the workspace-seed flow

### Stage 2: Dependency

Purpose:

- capture the environment after deterministic bootstrap such as dependency
  install, generated fixtures, and language-specific setup

Suggested key:

```text
dependency_stage_key = H(
  workspace_stage_key,
  compiled_policy_hash,
  toolchain_manifest_digest,
  resolved_tool_versions_digest,
  lockfile_inputs_digest,
  lockfile_parser_version,
  bootstrap_recipe_digest
)
```

Output:

- sealed rootfs snapshot ready for common build or test commands

Notes:

- this is the primary "nearly instant environment" target
- a warm hit should bypass repository clone and dependency install entirely

### Stage 3: Writable execution child

Purpose:

- provide disposable isolation for one sandbox lifetime

Suggested key:

- none; this is not a shared stage cache

Output:

- per-sandbox writable child clone of a dependency stage or workspace stage

Notes:

- this stage must never be shared or promoted automatically

## Transport Caches

Transport caches accelerate repository and registry access but are not
themselves runnable environments.

### Git transport cache

Purpose:

- reduce repeated upstream git traffic
- keep auth and fetch control on the host side

Suggested key:

```text
git_mirror_key = H(canonical_remote_url)
```

Output:

- host-side bare mirror
- optionally, a host-side protocol/content cache for repeated git fetch traffic

Notes:

- the existing git mirror is the current transport cache
- the gateway git caching work proposed in PR #138 belongs in this plane
- freshness must still block until the requested commit exists

### Registry/package transport cache

Purpose:

- cache fetched registry artifacts by verified content
- reduce repeated OCI/blob/tag traffic
- support future lockfile-derived allowlists without making the registry cache
  itself a runnable environment

Suggested key:

```text
registry_artifact_key = H(
  ecosystem,
  canonical_registry,
  package_identity,
  version,
  integrity_hash
)
```

Output:

- immutable blob plus verified metadata

Notes:

- URL alone is not sufficient
- the gateway OCI/content-cache work proposed in PR #138 belongs in this plane
- if a lockfile does not include strong integrity metadata, that ecosystem does
  not qualify for poisoning-resistant warm reuse in strict mode

## Stage Key Resolution

At request time, the control plane should derive the canonical stage keys from
the resolved request inputs.

The control plane can then resolve the highest reusable stage cache it trusts:

1. dependency stage hit
2. workspace stage hit
3. runtime stage hit
4. cold path

## Production Flow

Shared stage-cache publication should be a host-controlled promotion pipeline.

### Publish workspace stage

1. Resolve the exact repository checkout inputs.
2. Start from the selected runtime stage.
3. Materialize the repository through the host git gateway.
4. Verify the checkout:
   - remote URL canonicalized as expected
   - `HEAD` equals requested commit
   - submodules match requested policy
5. Snapshot the resulting root volume.
6. Publish the workspace-stage metadata and storage reference atomically.

### Publish dependency stage

1. Start from a published workspace stage.
2. Run deterministic bootstrap in a temporary builder sandbox.
3. Allow package fetches only through the gateway and only for lockfile-derived
   artifacts.
4. Verify:
   - no lockfile violations
   - no unexpected network access
   - bootstrap recipe completed successfully
5. Snapshot the resulting root volume.
6. Publish the dependency-stage metadata and storage reference atomically.

## Consumption Flow

Warm-hit resolution should be simple.

1. Resolve request inputs into the canonical stage keys.
2. Look up the best `ready` stage cache.
3. Clone its rootfs snapshot into a fresh writable child volume.
4. Attach that single writable child as the VM root disk.
5. Boot a fresh sandbox.

If no dependency stage exists but a workspace stage does:

1. clone the workspace stage
2. run deterministic bootstrap
3. optionally publish a dependency stage if the policy allows promotion

If only the runtime stage exists:

1. clone the runtime stage
2. perform repository checkout
3. continue upward through the same promotion flow

## Publication State Machine

Each stage cache entry should have an explicit lifecycle state.

For the first slice, keep the durable states minimal:

- `ready`
- `failed`
- `garbage`

Rules:

- only `ready` entries are reusable
- `ready` entries are immutable
- failed builds do not overwrite or downgrade existing `ready` entries
- promotion is atomic: the metadata entry becomes visible only after the
  storage reference is durable and verified

In-flight build coordination can stay outside the durable metadata record at
first if that keeps the implementation simpler.

## Trust and Safety Model

### Supply-chain and cache-poisoning constraints

The design can sharply reduce mutable trust at startup, but not remove all
upstream trust entirely.

Trusted inputs still include:

- the pinned base image digest
- the pinned git commit
- the lockfile integrity metadata
- the lockfile and toolchain parsers

The design should eliminate these weaker trust paths:

- mutable tags
- background refresh races against exact commit checkout
- package downloads accepted by URL or version string alone
- shared writable caches modified by untrusted sandboxes

### Publish safety rules

- publish only from trusted host-controlled flows
- verify bytes before storing by integrity hash
- use `write temp -> fsync -> atomic rename` for new artifact blobs and metadata
- never serve partially written artifacts
- never accept a guest-provided "cache hit" claim as authoritative

### Offline warm mode

When a policy requires strict offline warm-cache execution:

- the control plane must refuse upstream git or registry access
- only published `ready` stage caches may be used
- missing artifacts fail closed

## Runtime Filesystem Model

The default runtime model should use one guest-visible root filesystem.

That means:

- no default runtime union mount stack
- no default "base volume plus repo volume plus deps volume" mount assembly
- no separate default `/workspace` guest-visible volume
- one attached writable root volume per sandbox

Logical cache layering still exists, but it is represented in metadata and
lineage rather than in a live guest mount stack.

This matches the current backend shape better:

- Firecracker attaches one root drive to the microVM
- `darwin-vz` attaches one writable rootfs image per VM
- the snapshot store already models immutable rootfs states

## Firecracker Performance Implications

For Firecracker, the cache plan only pays off if rootfs materialization stops
doing a full ext4 copy on the hot path.

Current state:

- normal execution already uses the volume-store rootfs path
- snapshot-backed sandbox restore already feeds `storage_ref` through the same
  writable-root-volume helper
- clone-based hot hits still require a clone-capable driver such as ZFS
- the default `file` driver still performs full file copies

Recommended direction from here:

- keep using the shared volume-store rootfs path for all Firecracker launch
  modes
- prefer clone-capable Linux storage such as ZFS for warm-hit materialization
- measure clone latency separately from VM boot latency so driver choice is
  visible in benchmarks

Desired hot path:

1. resolve best stage cache
2. clone snapshot to writable child volume
3. attach child volume as Firecracker root disk
4. boot VM

Undesired hot path:

1. resolve best stage cache
2. copy full ext4 file to `rootfs-ephemeral.ext4`
3. boot VM

The first path makes warm hits dominated by clone metadata work plus VM boot.
The second path keeps warm hits bounded by rootfs size and host I/O.

## Relationship To Existing Plans

This plan composes with the existing documents rather than replacing them.

- `repository-bootstrap.md`
  defines exact-commit repository resolution and the host mirror-backed git
  gateway
- `snapshot-restore-fork.md`
  defines immutable rootfs snapshots and fresh-boot fan-out semantics
- `host-gateway.md`
  defines the host-side mediation point for git and registry traffic
- `firecracker-privilege-runtime.md`
  defines the Linux host-runtime direction needed for clone-capable snapshot
  storage and reduced root surface
- PR #138
  proposes the gateway Git/OCI content-cache transport plane that should remain
  separate from system stage caches

## Code Touchpoints and Planned Additions

| File | Change |
|---|---|
| `internal/gateway/mirror.go` | Already acts as the host-side git transport cache keyed by canonical remote URL. |
| `internal/repositorycheckout/checkout.go` | Already uses exact remote URL and full commit SHA as the checkout source of truth; `branch` currently affects local checkout mode only. |
| `internal/controlservice/workspace_seed.go` | Current workspace-stage orchestration lives here under workspace-seed naming; this should migrate to dedicated stage-cache metadata. |
| `internal/snapshotstore/store.go` | Should remain the user snapshot store rather than being expanded into the system-cache store. |
| `internal/volumestore/store.go` | Already provides the backend-neutral clone/snapshot contract shared by both backends. |
| `internal/backend/firecracker/backend.go` | Already routes normal execution and snapshot restore through writable root volume preparation; actual clone behavior depends on the configured driver. |
| `internal/backend/darwinvz/backend_darwin.go` | Already fits the same one-rootfs model and can use APFS clone materialization. |
| `internal/policy/policy.go` | Still needs lockfile-derived artifact allowlists and strict offline warm-cache requirements. |
| `internal/cachestore/*` | Planned new package for system-managed stage-cache metadata, separate from user snapshots. |

## Suggested Cache Metadata Model

Suggested stage-cache record shape:

```text
cache_key
stage                    // runtime | workspace | dependency
state                    // ready | failed | garbage
parent_cache_key
storage_driver
storage_ref
policy_hash
repository
input_manifest_digest
created_at
last_used_at
producer_version
```

This metadata should live in a dedicated `cachestore`, not in `snapshotstore`.
Input manifests should be stored as explicit structured metadata rather than as
opaque prose so determinism can be tested directly.

## Implementation Order

### Done

1. Keep the git mirror as the transport cache for exact-commit checkout.
2. Publish workspace-stage caches keyed by runtime stage plus exact repository
   inputs.

### Partial

1. Move Firecracker warm-hit materialization onto the shared writable-root
   volume path.
2. Use published snapshot `storage_ref` values as the source for writable child
   preparation.

These are architecturally landed, but the full performance win still depends on
clone-capable storage instead of the default `file` driver.

### Remaining

1. Add a dedicated `cachestore` for system-managed stage caches and canonical
   key derivation helpers.
2. Move workspace-stage metadata off reserved snapshot names and into
   `cachestore`.
3. Add lockfile parsing and one strict dependency stage for a single ecosystem
   first.
4. Add strict offline warm-cache mode and fail-closed launch checks.
5. Add garbage collection and retention policies after the key model and
   publication flow are stable.
6. Revisit cross-host distribution/export only after the local host model is
   proven worthwhile.

## Testing Plan

### Key determinism

- identical inputs produce identical keys
- irrelevant field ordering does not change keys
- parser version changes do change keys when parser behavior is versioned

### Publication safety

- partially written stage-cache entries are never visible as `ready`
- failed publishes do not corrupt existing entries
- concurrent publishes of the same key coalesce correctly

Current coverage already exists for the narrower workspace-stage flow, which is
implemented today as the workspace-seed flow:

- warm workspace-stage hits reuse snapshot-backed sandbox creation
- runtime-base changes invalidate workspace-stage reuse
- restore failures fall back to cold bootstrap and republish
- writable-volume preparation cleans up failed clones and uses the configured
  volume-store driver

### Policy enforcement

- lockfile-derived artifact allowlists block undeclared package requests
- offline warm mode fails closed when a required artifact is missing
- git repository materialization blocks until the requested commit exists in the
  mirror

### Runtime performance

- warm dependency-stage hit skips repository clone and dependency install
- Firecracker warm hits avoid full rootfs file copies
- snapshot clone latency and VM boot latency are measured separately

## Open Questions

- Which ecosystem should be the first strict lockfile-enforced package cache:
  npm, pip, or another?
- Should dependency-stage caches be published automatically on successful
  bootstrap, or only when explicitly requested?
- What retention policy should apply to large dependency-stage caches relative
  to smaller workspace-stage caches?
- Should workspace-stage identity continue to include the local checkout branch,
  or should branch stay outside reusable cache keys once checkout mode is
  modeled separately?
- Do we need a specialized optional read-only guest-visible artifact volume for
  very large immutable datasets, or can the single-rootfs model handle the
  first production slice cleanly?
- When we later revisit cross-host distribution, should exported stage caches be
  rebuilt from transport caches or exported as separate portable artifacts?
