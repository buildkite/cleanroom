# Layered Cache Plan

**Spec reference:** `spec.md` sections 5.1.1, 5.2, 6.4
**Status:** Proposed

## Summary

Build a deterministic cache hierarchy for environment construction, then
materialize the selected cached environment as a single writable root volume
per sandbox.

The core model is:

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

## Delivery Strategy

This plan should land in phases rather than as one large cross-backend change.

### Phase 1: Workspace-seed orchestration

Land the backend-neutral control-plane slice first:

- publish workspace-seed snapshots after exact-commit repository bootstrap
- look up matching workspace-seed snapshots before repeating bootstrap
- prove deterministic cache keying and trusted publication semantics

This phase improves warm repo-aware sandbox creation by skipping repeated
checkout work on snapshot-capable persistent backends.

It does **not** yet deliver the full hot-path performance win for Firecracker,
because Firecracker's normal execution flow still copies the prepared ext4
rootfs per run.

### Phase 2: Firecracker hot-path materialization

Follow with a Firecracker-specific runtime change:

- replace the per-run ext4 file copy with clone-based writable root volume
  preparation
- consume published cache artifacts through the volume-store clone path

This is the phase that converts workspace-seed hits from "skip git/bootstrap
work" into "nearly plain sandbox boot time" on Firecracker.

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

What is still missing is a unified cache model that answers:

- which environment artifacts should exist
- which exact inputs produce them
- who is allowed to publish them
- when a cache hit is trusted
- how warm hits become close to plain sandbox creation time

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
- Guaranteeing zero supply-chain risk in an absolute sense.

## Design Principles

### 1. Keys must be derived from immutable inputs

Reusable environment artifacts must never be keyed by:

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

### 2. Untrusted sandboxes must not publish shared cache entries

A guest workload can request a checkpoint or complete a bootstrap step, but the
host control plane remains authoritative for promotion into a shared cache.

That means:

- build into a temporary sandbox or temporary snapshot
- verify the result on the host side
- publish atomically
- never mutate a `ready` cache entry in place

### 3. Treat caches as immutable artifacts, not writable workspaces

Every reusable layer should be modeled as an immutable artifact with metadata,
state, lineage, and a storage reference.

Mutable work happens only in:

- a temporary builder sandbox
- a per-run writable child clone

### 4. Keep cache layers host-side and guest runtime simple

The guest should not see a live mount stack like "base + repo + deps" for the
default case. That adds backend-specific mount complexity, ordering issues, and
new attack surface without improving deterministic reuse.

The default runtime contract should remain:

- clone one sealed rootfs snapshot into a writable child
- attach that writable child as the VM root disk
- boot a fresh sandbox

### 5. Separate transport caches from environment caches

A git mirror or a package artifact CAS improves fetch cost and upstream load,
but it is not itself a runnable environment.

That distinction matters for correctness:

- transport caches are host-side accelerators
- environment caches are sealed rootfs states that can be cloned into sandboxes

## Cache Planes

We should model two planes explicitly.

### A. Logical cache plane

This plane answers:

- what artifact exists
- what inputs produced it
- whether it is safe to reuse
- how it relates to parent artifacts

Examples:

- git mirror
- package artifact blob
- workspace seed
- dependency seed

### B. Physical materialization plane

This plane answers:

- where the artifact bytes live
- how they are cloned
- what gets attached to the VM

Examples:

- plain file copy
- APFS `clonefile`
- ZFS clone of a zvol snapshot

The logical cache plane should not depend on one storage backend. The physical
materialization plane is backend-specific and may vary by host.

## Cache Layers

### Layer 0: Runtime base

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

- immutable prepared rootfs artifact
- optionally stored as a managed base volume

Notes:

- this is not repository-specific
- this is the first layer that should be reused aggressively across runs

### Layer 1: Git mirror

Purpose:

- reduce repeated upstream git traffic
- keep auth and fetch control on the host side

Suggested key:

```text
git_mirror_key = H(canonical_remote_url)
```

Output:

- host-side bare mirror

Notes:

- this is a transport cache, not a runnable environment
- freshness must still block until the requested commit exists

### Layer 2: Package artifact CAS

Purpose:

- cache fetched registry artifacts by verified content
- enforce lockfile-derived allowlists

Suggested key:

```text
artifact_key = H(
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
- if a lockfile does not include strong integrity metadata, that ecosystem does
  not qualify for poisoning-resistant warm reuse in strict mode

### Layer 3: Workspace seed

Purpose:

- capture an exact checked-out repository state ready for command execution

Suggested key:

```text
workspace_seed_key = H(
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

### Layer 4: Dependency seed

Purpose:

- capture the environment after deterministic bootstrap such as dependency
  install, generated fixtures, and language-specific setup

Suggested key:

```text
dependency_seed_key = H(
  workspace_seed_key,
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

### Layer 5: Writable execution child

Purpose:

- provide disposable isolation for one sandbox lifetime

Suggested key:

- none; this is not a shared cache artifact

Output:

- per-sandbox writable child clone of a dependency seed or workspace seed

Notes:

- this layer must never be shared or promoted automatically

## Environment Key Resolution

At request time, the control plane should derive a single canonical target key
from the resolved request inputs.

Illustrative shape:

```text
env_key = H(
  schema_version,
  backend_name,
  architecture,
  runtime_base_key,
  compiled_policy_hash,
  canonical_remote_url,
  commit_sha,
  submodule_resolution_digest,
  toolchain_manifest_digest,
  resolved_tool_versions_digest,
  lockfile_inputs_digest,
  lockfile_parser_version,
  bootstrap_recipe_digest
)
```

The control plane can then resolve the highest reusable artifact it trusts:

1. dependency seed hit
2. workspace seed hit
3. runtime base hit
4. cold path

## Production Flow

Shared cache publication should be a host-controlled promotion pipeline.

### Publish workspace seed

1. Resolve the exact repository checkout inputs.
2. Start from the selected runtime base.
3. Materialize the repository through the host git gateway.
4. Verify the checkout:
   - remote URL canonicalized as expected
   - `HEAD` equals requested commit
   - submodules match requested policy
5. Snapshot the resulting root volume.
6. Publish the workspace seed metadata and storage reference atomically.

### Publish dependency seed

1. Start from a published workspace seed.
2. Run deterministic bootstrap in a temporary builder sandbox.
3. Allow package fetches only through the gateway and only for lockfile-derived
   artifacts.
4. Verify:
   - no lockfile violations
   - no unexpected network access
   - bootstrap recipe completed successfully
5. Snapshot the resulting root volume.
6. Publish the dependency seed metadata and storage reference atomically.

## Consumption Flow

Warm-hit resolution should be simple.

1. Resolve request inputs into the canonical environment key.
2. Look up the best `ready` artifact.
3. Clone its rootfs snapshot into a fresh writable child volume.
4. Attach that single writable child as the VM root disk.
5. Boot a fresh sandbox.

If no dependency seed exists but a workspace seed does:

1. clone the workspace seed
2. run deterministic bootstrap
3. optionally publish a dependency seed if the policy allows promotion

If only the runtime base exists:

1. clone the runtime base
2. perform repository checkout
3. continue upward through the same promotion flow

## Publication State Machine

Each cache artifact should have an explicit lifecycle state.

Suggested states:

- `pending`
- `building`
- `verifying`
- `ready`
- `failed`
- `garbage`

Rules:

- only `ready` entries are reusable
- `ready` entries are immutable
- failed builds do not overwrite or downgrade existing `ready` entries
- promotion is atomic: the metadata entry becomes visible only after the
  storage reference is durable and verified

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
- only published `ready` artifacts may be used
- missing artifacts fail closed

## Runtime Filesystem Model

The default runtime model should use one guest-visible root filesystem.

That means:

- no default runtime union mount stack
- no default "base volume plus repo volume plus deps volume" mount assembly
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

Recommended direction:

- use the existing volume-store rootfs path for normal execution, not only for
  persistent sandboxes and snapshot restore
- treat a published cache artifact's `storage_ref` as the source for
  `CloneSnapshotToVolume`
- prefer a clone-capable Linux driver such as ZFS for warm-hit materialization

Desired hot path:

1. resolve best cache artifact
2. clone snapshot to writable child volume
3. attach child volume as Firecracker root disk
4. boot VM

Undesired hot path:

1. resolve best cache artifact
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

## Existing Code Touchpoints

| File | Change |
|---|---|
| `internal/gateway/mirror.go` | Keep as the host-side git transport cache keyed by canonical remote URL. |
| `internal/repositorycheckout/checkout.go` | Continue using exact remote URL and commit as the checkout source of truth. |
| `internal/snapshotstore/store.go` | Extend snapshot-adjacent metadata to record cache layer type, key, parent, inputs, and publication state. |
| `internal/volumestore/store.go` | Keep the abstract clone/snapshot interface; do not leak cache semantics into the storage driver API. |
| `internal/backend/firecracker/backend.go` | Move the normal execution hot path onto clone-based root volume preparation. |
| `internal/backend/darwinvz/backend_darwin.go` | Reuse the same logical cache metadata while keeping APFS-backed clone materialization. |
| `internal/policy/policy.go` | Extend compiled policy data with lockfile-derived artifact allowlists and strict offline mode requirements. |

## Suggested Metadata Model

Suggested artifact record shape:

```text
artifact_key
artifact_type            // runtime_base | workspace_seed | dependency_seed
state                    // pending | building | verifying | ready | failed | garbage
backend
architecture
policy_hash
parent_artifact_key
storage_driver
storage_ref
input_manifest_digest
created_at
producer_version
```

Input manifests should be stored as explicit structured metadata rather than as
opaque prose so determinism can be tested directly.

## Implementation Order

1. Add cache metadata records and canonical key derivation helpers.
2. Keep the git mirror as the transport cache for exact-commit checkout.
3. Publish workspace seeds keyed by exact repository inputs.
4. Move Firecracker warm-hit materialization onto clone-based root volume
   preparation.
5. Add lockfile parsing and package artifact CAS for one ecosystem first.
6. Publish dependency seeds keyed by lockfiles, toolchains, and bootstrap
   recipe.
7. Add strict offline warm-cache mode and fail-closed launch checks.
8. Add garbage collection and retention policies after the key model and
   publication flow are stable.

## Testing Plan

### Key determinism

- identical inputs produce identical keys
- irrelevant field ordering does not change keys
- parser version changes do change keys when parser behavior is versioned

### Publication safety

- partially written artifacts are never visible as `ready`
- failed publishes do not corrupt existing entries
- concurrent publishes of the same key coalesce correctly

### Policy enforcement

- lockfile-derived artifact allowlists block undeclared package requests
- offline warm mode fails closed when a required artifact is missing
- git repository materialization blocks until the requested commit exists in the
  mirror

### Runtime performance

- warm dependency-seed hit skips repository clone and dependency install
- Firecracker warm hits avoid full rootfs file copies
- snapshot clone latency and VM boot latency are measured separately

## Open Questions

- Which ecosystem should be the first strict lockfile-enforced package cache:
  npm, pip, or another?
- Should dependency seeds be published automatically on successful bootstrap, or
  only when explicitly requested?
- What retention policy should apply to large dependency seeds relative to
  smaller workspace seeds?
- Do we need a specialized optional read-only guest-visible artifact volume for
  very large immutable datasets, or can the single-rootfs model handle the
  first production slice cleanly?
