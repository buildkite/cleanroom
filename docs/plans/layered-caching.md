# Layered Cache Plan

**Spec reference:** `spec.md` sections 5.1.1, 5.2, 6.4
**Status:** In progress
**Last reviewed:** 2026-05-05

## Summary

Build an input-addressed, host-local cache pipeline for environment
construction,
then
materialize the selected cached environment as a single writable root volume
per sandbox.

This plan intentionally keeps three systems separate:

- user snapshots are explicit, user-facing savepoints
- user changesets are explicit, host-local packages of local repository changes
- system caches are implicit, host-managed stage outputs

The system-cache pipeline uses stage terminology:

- runtime stage: prepared runtime rootfs
- workspace stage: runtime rootfs plus exact repository checkout
- dependency stage: workspace stage plus policy-constrained bootstrap and
  dependencies
- portable dependency stage: dependency-prepared rootfs that can be restored
  first and then have the repository checkout refreshed to a different commit
  when declared dependency inputs still match
- services stage: dependency stage or workspace stage plus policy-constrained
  service preparation state on disk

This document remains the plan for full-rootfs system stage caches. The
declared input/output model for dependency and service output volumes now lives
in `dependency-service-volume-caches.md`. That block-volume model is the path
for repo-local outputs such as `node_modules`, package stores, generated files,
and service data directories. The older idea of a standalone
`internal/inputmanifest` package did not become the implementation; current
input digests are computed in the control service from the repository tree or
explicit changeset.

Each stage output is a full sealed rootfs snapshot. Higher stages subsume lower
stages. At runtime we do not mount separate runtime, workspace, dependency, and
services layers as a live stack. We clone the best ready stage into one writable
child and boot that as the only guest root disk. When a portable dependency
stage is used, it is still a full rootfs snapshot. Cleanroom restores it into a
writable child, refreshes the repository checkout inside that child, and only
then treats the sandbox as ready.

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

We need both. Input-addressed caches without cheap clone materialization still
leave startup expensive. Cheap block clones without deterministic key
derivation still leave reuse unsafe and hard to reason about.

## Terminology

This document uses:

- **stage** for the transformation step in the cache pipeline
- **cache entry** for the immutable stored output of a stage
- **portable dependency stage** for a dependency-prepared full-rootfs cache entry
  whose useful dependency state is expected to survive a later repository
  checkout refresh

This document and the current implementation use stage terminology for system
caches. The dependency-stage `reuse_mode` value `portable` is metadata for the
portable dependency stage rather than a separate user-facing cache type.

## Delivery Strategy

This plan should land in phases rather than as one large cross-backend change.

### Phase 1: Workspace-stage orchestration

Status: largely landed for snapshot-capable backends.

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

For release framing, Linux CI and local Linux should be described with the same
model: machine bootstrap plus unprivileged user workflow. The difference is not
"CI mode" versus "local mode". The difference is whether the host has already
been prepared with the Firecracker helper, sudoers access, KVM access, and an
optional Cleanroom ZFS dataset root.

### Phase 3: Dedicated cache store and dependency stage

Status: started.

The dedicated system-cache store is now in place, and the first dependency
stage slice is beginning with a single configured dependency bootstrap flow.

This phase now means:

- keep `cachestore` as the system-managed stage store and `snapshotstore` as
  the user snapshot store
- keep workspace-stage metadata in `cachestore`, not in reserved snapshot names
- add one explicit dependency stage with a declared bootstrap command and key
  files before generalizing further
- start with `sandbox.dependencies.command` plus
  `sandbox.dependencies.key.files`, which bootstraps a single dependency
  command inside the restored workspace stage and publishes the resulting
  dependency stage
- add a portable dependency-stage path for dependency state that survives an
  in-place checkout refresh, so unchanged lockfiles can reuse a dependency
  snapshot across source-only commits
- keep distribution/export out of scope until the host-local model is proven

## Current Progress Snapshot

### Landed

- host-side git mirror transport cache keyed by canonical remote URL
- gateway Git/OCI transport cache via content-cache
- exact-commit repository bootstrap through the host-controlled flow
- runtime-base-key derivation for `firecracker` and `darwin-vz`
- dedicated `cachestore` for system-managed stage outputs
- workspace-stage publish, lookup, restore, and fallback republish in the
  control service
- workspace-stage metadata moved out of reserved snapshot naming and into
  `cachestore`
- one writable-root-volume preparation path for Firecracker normal execution
  and snapshot restore
- request-scoped `--copy-in` packaging and replay after exact
  repository checkout
- durable host-local `changesetstore` metadata and payload storage for
  explicit repository changesets, with stage-cache metadata recording the
  stable changeset ID
- dependency-stage and services-stage key-file digest resolution from the
  post-apply tree when an explicit changeset is present
- exact dependency-stage publish, lookup, restore, and republish for one
  configured dependency bootstrap command
- portable dependency-stage reuse as an explicit
  `sandbox.dependencies.reuse: portable` mode for declared key-file inputs
- services-stage publish, lookup, restore, and republish on top of the selected
  workspace or dependency stage

### Partial

- Firecracker hot-path materialization is wired through the volume-store path,
  but clone-based behavior still depends on the configured storage driver
- dependency-stage keying has the first explicit input model, but richer
  toolchain-derived inputs, lockfile parser inputs, and artifact allowlists are
  still pending
- explicit local changes are still supplied through each create request today;
  the durable `changesetstore` records the replay payload and identity, but a
  user-facing `--changeset <id>` reuse surface has not been added yet

Today that means:

- ZFS-backed Firecracker is the supported Linux layered-cache path
- file-backed Firecracker remains functional, but warm restores are degraded
  because they still materialise writable root volumes by copying bytes
- `cleanroom doctor` should be treated as the source of truth for whether a
  given Linux host is in the supported, degraded, or unavailable Firecracker
  state

### Not started

- strict offline warm-cache mode
- garbage collection and retention policy
- cross-host distribution/export for stage caches

### Current caveats

- workspace-stage keying currently includes the local checkout branch because
  repository bootstrap can create either a detached checkout or a named local
  branch
- the dependency-stage key intentionally starts from the workspace-stage key
  plus policy hash, dependency bootstrap recipe digest, and declared key-file
  digests; richer toolchain and lockfile-parser inputs are still to come
- because dependency-stage snapshots are full rootfs snapshots, decoupling them
  from exact workspace identity requires a checkout refresh and validation step
  after restore; it is not live layer composition

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
- Making system stage caches user-addressable through the normal snapshot
  restore surface such as `--from`.
- Guaranteeing zero supply-chain risk in an absolute sense.

## Design Principles

### 1. Keep user snapshots, user changesets, and system caches separate

User snapshots, user changesets, and system caches may use the same low-level
clone/snapshot and content-addressed storage mechanisms, but they are different
products:

- user snapshots are addressed by snapshot ID and user intent
- user changesets are addressed by explicit replayable repository inputs such as
  `(base checkout, changeset digest)`
- system caches are addressed by `(stage, key)` and immutable, replayable
  inputs
- user snapshots are explicit and durable
- user changesets are explicit and replayable, but should remain host-local in
  the first slice
- system caches are implicit and eligible for automatic garbage collection

`snapshotstore` should remain the user snapshot store. System-managed stage
outputs should live in a dedicated `cachestore`. Explicit local repository
changesets should live in a separate `changesetstore`.

The user-facing restore/fork surface should remain snapshot-oriented:

- `--from` should refer to user snapshots, not system stage caches
- normal sandbox creation should resolve system stage caches automatically from
  request inputs
- explicit local changes should use a separate changeset surface such as
  `--changeset` or `--copy-in`, not snapshot restore semantics
- if direct stage-cache selection is ever exposed, it should use a separate
  operator/debug surface rather than sharing snapshot semantics

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
- explicit changeset digest and resulting tree digest when local changes are
  included
- lockfile bytes and lockfile parser version
- toolchain manifest bytes and resolved tool versions
- compiled policy hash
- bootstrap recipe digest

Portable dependency stages are the one deliberate exception to "commit SHA is
part of every repository cache key": they may omit the exact source commit only
because they include declared dependency key-file bytes and must refresh and
verify the checkout before execution.

### 3. System caches are stage outputs

The cache pipeline should be described as ordered stages:

- runtime
- workspace
- dependency
- services

Each stage produces one immutable cached output. Higher stages subsume the
lower stages logically, even if the runtime only boots from one concrete rootfs
snapshot.

Portable dependency stages are dependency-stage entries with different reuse
semantics. They do not mean Cleanroom can compose independent workspace,
dependency, and services snapshots at runtime.

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

We should model four distinct things explicitly.

### A. System stage caches

This is the host-managed environment cache pipeline:

- runtime stage cache
- workspace stage cache
- dependency stage cache
  - including portable dependency-stage entries with `reuse_mode: portable`
- services stage cache

These are reusable rootfs states that can be cloned into disposable sandboxes.
Portable dependency-stage entries are still full rootfs states; their
portability comes from checkout refresh and validation, not from filesystem
layering.

### B. Transport caches

These reduce upstream fetch cost but are not runnable environments:

- git mirror transport cache
- gateway git response cache
- gateway OCI/blob cache

The existing git mirror is already in-tree. The gateway Git/OCI content-cache
layer now lives under `internal/gateway` and fits here as a transport-cache
plane, not as a system stage cache.

### C. Runtime materialization

This is how a selected stage cache becomes one writable VM root disk:

- plain file copy
- APFS `clonefile`
- ZFS snapshot/clone

Stage cache selection is backend-neutral. Materialization remains
backend-specific.

### D. Explicit user changesets

This is the user-facing package for local modifications on top of an exact
repository checkout:

- base checkout identified by canonical remote URL, full commit SHA, and
  submodule mode
- deterministic changeset digest and resulting tree digest
- versioned transport payload for replay on another sandbox create

These are not runnable environments and should not be addressed through
snapshot restore or stage-cache selection.

They should:

- be explicit and opt-in
- remain host-local first
- participate in workspace-stage and dependency-stage key resolution
- be applied after exact repository checkout and before dependency bootstrap

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

- capture an exact checked-out repository state, optionally with an explicit
  changeset applied, ready for command execution

Suggested key:

```text
workspace_stage_key = H(
  runtime_base_key,
  canonical_remote_url,
  commit_sha,
  submodule_mode,
  submodule_resolution_digest,
  changeset_digest,
  checkout_mode,
  repository_destination_dir,
  materialization_recipe_digest
)
```

Output:

- sealed rootfs snapshot containing the exact repository checkout, optionally
  with the requested changeset already applied

Notes:

- this is the next speed stage after mirror-backed clone
- it turns "avoid hammering upstream" into "skip clone and checkout on warm hit"
- `commit_sha` identifies the repo tree, but it is not enough to identify the
  full workspace stage by itself
- when a changeset is present, workspace-stage identity must include the
  changeset digest rather than silently treating the dirty worktree as part of
  the base checkout
- workspace-stage identity must also capture repository provenance and checkout
  behavior such as submodule resolution, destination path, and any explicit
  materialization recipe details such as sparse checkout or LFS hydration
- current implementation uses the workspace-stage flow

### Stage 2: Dependency

Purpose:

- capture the environment after policy-constrained bootstrap such as
  dependency install, generated fixtures, and language-specific setup

There are two dependency-cache modes.

The conservative exact dependency stage remains tied to a specific workspace:

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

The portable dependency stage is keyed by dependency-relevant inputs rather than
by the exact workspace snapshot:

```text
portable_dependency_stage_key = H(
  runtime_base_key,
  backend_family,
  canonical_remote_url,
  repository_destination_dir,
  repository_submodule_mode,
  checkout_refresh_recipe_digest,
  dependency_policy_hash,
  dependency_key_files_digest,
  bootstrap_recipe_digest,
  dependency_output_mode,
  producer_version
)
```

Output:

- exact dependency stage: sealed rootfs snapshot ready for common build or test
  commands against one exact checkout
- portable dependency stage: sealed rootfs snapshot that may contain an older
  checkout, but can be restored first and then refreshed to the requested
  checkout before execution

Notes:

- this is the primary "nearly instant environment" target
- a warm hit should bypass repository clone and dependency install entirely
- this stage is only reproducible to the extent that the selected ecosystem,
  toolchain, and bootstrap recipe are actually constrained enough to produce
  stable outputs
- the first implementation slice should be explicit and narrow rather than
  heuristic; the current target is one configured dependency command and a set
  of declared repository key files on top of a workspace stage
- when a changeset is present, declared dependency key files must be resolved
  from the post-apply tree rather than only from the base commit mirror
- for that initial slice, the dependency-stage key can conservatively start
  with the workspace-stage key plus policy hash, the concrete bootstrap recipe
  digest, and declared key-file digests, then grow richer toolchain inputs
  later
- portable dependency stages are a separate reuse mode, not a replacement for
  exact dependency stages
- a portable dependency-stage hit must refresh the repository checkout inside
  the writable child before user code runs; the restored snapshot's old
  checkout is never treated as current
- after checkout refresh and changeset apply, Cleanroom must recompute the
  declared dependency key-file digest and only skip dependency bootstrap if it
  still matches the stage entry's recorded digest
- if the digest does not match, Cleanroom must discard the restored child and
  fall back to the normal workspace-stage path before running fresh dependencies
- the portable mode is suitable for dependency outputs that survive
  `git reset --hard` and `git clean -ffdx`, such as Go module caches,
  `mise` installs, system packages, and global package-manager caches
- repo-local outputs such as `node_modules`, `vendor/bundle`, or generated files
  under the checkout should stay on the exact dependency-stage path unless
  explicit preserve/output semantics are added later

### Stage 3: Services

Purpose:

- capture policy-constrained services-preparation state after dependency
  bootstrap
- keep reusable on-disk service setup separate from per-execution process
  startup

Suggested key:

```text
services_stage_key = H(
  parent_stage_key,
  compiled_policy_hash,
  services_key_files_digest,
  services_bootstrap_recipe_digest
)
```

Output:

- sealed rootfs snapshot containing prepared service state on top of the
  selected dependency stage or workspace stage

Notes:

- this stage stores on-disk state only, not live processes or memory
- if no dependency stage is configured, it can start from a workspace stage
- services-stage identity must include the exact parent stage key so service
  preparation is tied to the dependency or workspace state it was built from

### Materialization: Writable execution child

Purpose:

- provide disposable isolation for one sandbox lifetime

Suggested key:

- none; this is not a shared stage cache

Output:

- per-sandbox writable child clone of a services stage, dependency stage, or
  workspace stage

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
- the merged gateway git/content-cache layer belongs in this plane
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
- the merged gateway OCI/content-cache transport layer belongs in this plane
- if a lockfile does not include strong integrity metadata, that ecosystem does
  not qualify for poisoning-resistant warm reuse in strict mode

## Stage Key Resolution

At request time, the control plane should derive the canonical stage keys from
the resolved request inputs.

The control plane can then resolve the highest reusable stage cache it trusts:

1. services stage hit for the exact checkout plus optional changeset
2. dependency stage hit for the exact checkout plus optional changeset
3. portable dependency-stage hit for matching dependency inputs, followed by
   checkout refresh, optional changeset apply, and post-refresh key-file
   validation
4. workspace stage hit for the exact checkout plus optional changeset
5. exact-checkout workspace stage hit, followed by verified changeset apply
6. runtime stage hit
7. cold path

Portable dependency-stage lookup should use host-side key-file digest resolution
when possible so Cleanroom does not restore a candidate that is already known to
be stale. The post-refresh digest check is still required as defense in depth
before skipping dependency bootstrap.

## Production Flow

Shared stage-cache publication should be a host-controlled promotion pipeline.

### Package explicit changeset

1. Resolve the exact base checkout inputs:
   - canonical remote URL
   - full commit SHA
   - submodule mode
2. Compute a deterministic manifest of local modifications relative to that
   base checkout.
3. Build a versioned transport payload that can replay those modifications onto
   the exact checkout.
4. Compute a stable changeset identity from the base checkout plus the resulting
   post-apply tree, not from patch serialization alone.
5. Persist the immutable changeset record and payload atomically in
   `changesetstore`.

### Publish workspace stage

1. Resolve the exact repository checkout inputs and any explicit changeset
   inputs.
2. Start from the selected runtime stage.
3. Materialize the repository through the host git gateway.
4. If a changeset was requested, apply it and verify the resulting tree digest.
5. Verify the checkout:
   - remote URL canonicalized as expected
   - `HEAD` equals requested commit
   - submodules match requested policy
6. Snapshot the resulting root volume.
7. Publish the workspace-stage metadata and storage reference atomically.

### Publish dependency stage

1. Start from a published workspace stage that already reflects the requested
   exact checkout and optional changeset.
2. Run the constrained bootstrap recipe in a temporary builder sandbox.
3. Allow package fetches only through the gateway and only for lockfile-derived
   artifacts.
4. Verify:
   - no lockfile violations
   - no unexpected network access
   - bootstrap recipe completed successfully
5. Snapshot the resulting root volume.
6. Publish the dependency-stage metadata and storage reference atomically.

### Publish portable dependency stage

1. Start from the same host-controlled dependency bootstrap flow as an exact
   dependency stage.
2. Compute the portable dependency stage key from the runtime base, repository
   identity, checkout-refresh recipe, dependency policy hash, bootstrap recipe
   digest, and declared dependency key-file digest.
3. Verify the bootstrap completed successfully and record the key-file digest
   that made the stage reusable.
4. Verify that the dependency output mode is portable. The first portable slice
   should only promote stage entries whose useful dependency state lives outside
   the repository checkout, or whose output paths are explicitly declared by a
   later policy surface.
5. Snapshot the resulting root volume.
6. Publish the portable stage metadata and storage reference atomically. The
   stored repository checkout is provenance only; later reuse must not treat it
   as the requested checkout.

### Publish services stage

1. Start from a published dependency stage when one exists, otherwise from a
   published workspace stage.
2. Run the constrained services-preparation recipe in a temporary builder
   sandbox.
3. Stop any long-lived processes before snapshot publication; the cached value
   is on-disk state, not live process memory.
4. Verify the services-preparation recipe completed successfully.
5. Snapshot the resulting root volume.
6. Publish the services-stage metadata and storage reference atomically.

## Consumption Flow

Warm-hit resolution should be simple.

1. Resolve request inputs into the canonical stage keys.
2. Look up the best `ready` stage cache.
3. Clone its rootfs snapshot into a fresh writable child volume.
4. Attach that single writable child as the VM root disk.
5. Boot a fresh sandbox.
6. Perform any required trusted post-restore work, such as checkout refresh for
   portable dependency stages, before reporting the sandbox as ready.

If no services stage exists but a dependency stage does:

1. clone the dependency stage
2. run the constrained services-preparation recipe
3. optionally publish a services stage if the policy allows promotion

If no exact dependency stage exists but a portable dependency stage does:

1. clone the portable dependency stage into a writable child
2. refresh the repository checkout in that child to the requested commit
3. if a changeset was requested, apply it and verify the resulting tree digest
4. recompute the declared dependency key-file digest from the refreshed tree
5. if the digest still matches the stage metadata, skip dependency bootstrap
   and run from the refreshed child
6. if the digest differs, discard the child and continue with the normal
   workspace-stage or cold path

This flow does not compose immutable snapshots. It restores one full snapshot
and mutates only the disposable writable child.

If no dependency stage or portable dependency stage exists but a workspace stage
does:

1. clone the workspace stage
2. if needed, apply and verify the requested changeset
3. run the constrained bootstrap recipe
4. optionally publish a dependency stage if the policy allows promotion
5. if needed, run the constrained services-preparation recipe
6. optionally publish a services stage if the policy allows promotion

If only the runtime stage exists:

1. clone the runtime stage
2. perform repository checkout
3. if a changeset was requested, apply and verify it
4. continue upward through the same promotion flow

This stage-cache resolution should remain an internal control-plane behavior for
normal users. User-facing restore/fork flows should continue to target explicit
snapshots rather than arbitrary stage-cache entries.

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
- never treat a portable dependency stage's stored checkout as current; checkout
  refresh and verification are part of every portable dependency-stage hit
- if post-refresh dependency key-file validation fails, discard the restored
  child instead of trying to repair it in place

### Offline warm mode

When a policy requires strict offline warm-cache execution:

- the control plane must refuse upstream git or registry access
- only published `ready` stage caches may be used
- missing artifacts fail closed

### Portable Dependency-Stage Safety

Portable dependency stages trade exact checkout identity for faster
source-iteration when declared dependency inputs are unchanged. The safety rules
are stricter than a normal exact dependency-stage hit:

- require declared dependency key files; an empty key-file set should remain on
  the exact workspace-bound path unless a stronger input model exists
- compute key-file digests from the requested checkout, including post-apply
  changesets
- restore the stage only into a fresh writable child
- refresh the checkout before user code runs
- verify `HEAD`, submodule behavior, and changeset tree digest after refresh
- recompute the key-file digest after refresh and compare it with the stage
  metadata
- do not promote or reuse portable dependency stages for dependency outputs
  under the checkout unless explicit output-preservation semantics define how
  those paths survive `git clean`

The main correctness risk is under-declared dependency inputs. If the bootstrap
command reads source files, scripts, generated config, or environment inputs
that are not represented in the dependency key, cross-commit reuse can be stale.
The conservative exact dependency stage should remain available for those cases.

## Runtime Filesystem Model

The default runtime model should use one guest-visible root filesystem.

That means:

- no default runtime union mount stack
- no default "base volume plus repo volume plus deps volume" mount assembly
- no separate default `/workspace` guest-visible volume
- one attached writable root volume per sandbox
- no live composition of independent workspace, dependency, and services
  snapshots

Logical cache layering still exists, but it is represented in metadata and
lineage rather than in a live guest mount stack.

Portable dependency stages keep the same filesystem model. A portable hit clones
one full rootfs snapshot into a writable child, then refreshes the checkout
inside that child. The old checkout inside the sealed stage is an implementation
detail, not part of the requested sandbox state.

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
- `internal/gateway/contentcache.go`
  implements the gateway Git/OCI transport-cache plane that should remain
  separate from system stage caches

## Code Touchpoints and Planned Additions

| File | Change |
|---|---|
| `internal/gateway/mirror.go` | Already acts as the host-side git transport cache keyed by canonical remote URL. |
| `internal/repositorycheckout/checkout.go` | Already uses exact remote URL and full commit SHA as the checkout source of truth; `branch` currently affects local checkout mode only. `BuildRefreshCommand` is also the checkout-refresh primitive needed after restoring a portable dependency stage. |
| `internal/controlservice/workspace_stage.go` | Current workspace-stage orchestration already lives here using dedicated stage-cache metadata. |
| `internal/controlservice/dependency_stage.go` | Full-rootfs dependency-stage orchestration entry point for policy-controlled bootstrap, declared key-file hashing, post-apply tree keying, exact dependency-stage publication, and the portable dependency-stage lookup/validation path. |
| `internal/controlservice/block_volume_*.go` | Implemented dependency and service block-volume cache planning, launch-time volume specs, isolated execution, escaped-write fallback, and publication for declared input/output blocks. |
| `internal/snapshotstore/store.go` | Should remain the user snapshot store rather than being expanded into the system-cache store. |
| `internal/changesetstore/*` | Landed store for explicit local changeset metadata and replay payloads, separate from both snapshots and stage caches. |
| `internal/volumestore/store.go` | Already provides the backend-neutral clone/snapshot contract shared by both backends. |
| `internal/backend/firecracker/backend.go` | Already routes normal execution and snapshot restore through writable root volume preparation; actual clone behavior depends on the configured driver. |
| `internal/backend/darwinvz/backend_darwin.go` | Already fits the same one-rootfs model and can use APFS clone materialization. |
| `internal/policy/policy.go` | Now carries ordered dependency and service blocks, declared block inputs and outputs, and `sandbox.docker.required`. Future work still needs richer toolchain inputs, artifact allowlists, and strict offline warm-cache requirements. |
| `internal/cachestore/*` | Landed package for system-managed stage-cache metadata, separate from user snapshots. |

## Suggested Cache Metadata Model

Suggested stage-cache record shape:

```text
cache_key
stage                    // runtime | workspace | dependency | services
reuse_mode               // exact | portable, when stage = dependency
state                    // ready | failed | garbage
parent_cache_key
changeset_id             // optional, when a workspace/dependency stage includes explicit local changes
storage_driver
storage_ref
policy_hash
repository
input_manifest_digest
dependency_key_files_digest
checkout_refresh_required
created_at
last_used_at
producer_version
```

This metadata should live in a dedicated `cachestore`, not in `snapshotstore`.
Current cache records store deterministic input digest metadata. The
block-volume implementation derives those digests through the control service's
repository and changeset-aware input-file digest helpers rather than a separate
manifest package.

Suggested changeset record shape:

```text
changeset_id
canonical_remote_url
base_commit_sha
submodule_mode
changeset_digest
final_tree_digest
transport_format
transport_ref
payload_digest
created_at
```

This metadata should live in a dedicated `changesetstore`, not in
`snapshotstore` or `cachestore`.

## Implementation Order

### Done

1. Keep the git mirror as the transport cache for exact-commit checkout.
2. Publish workspace-stage caches keyed by runtime stage plus exact repository
   inputs.
3. Add a dedicated `cachestore` for system-managed stage caches and move
   workspace-stage metadata off reserved snapshot names.
4. Add request-scoped local changeset packaging and replay on top of
   exact-commit workspaces.
5. Resolve dependency and services key-file digests from the post-apply tree
   when a changeset is present.
6. Add the first explicit dependency stage using one configured bootstrap
   command. The current slice is `sandbox.dependencies.command` plus
   `sandbox.dependencies.key.files`, keyed by workspace stage plus policy,
   command recipe, and declared key-file digests.
7. Add portable dependency-stage reuse for cross-commit iteration when declared
   dependency key files are unchanged. The current slice is opt-in through
   `sandbox.dependencies.reuse: portable`, restores the dependency-prepared
   rootfs into a writable child, refreshes the checkout to the requested commit,
   and falls back to normal dependency bootstrap if restore or refresh fails.
8. Add services-stage caching on top of the selected workspace or dependency
   stage.
9. Add a dedicated `changesetstore` for durable host-local changeset metadata
   and replay payloads, and record the stable changeset ID in system stage-cache
   metadata when a stage includes explicit local changes.
10. Add dependency and service block-volume caches for declared input/output
    blocks. These cover repo-local and service-data outputs that are a poor fit
    for portable full-rootfs dependency stages.

### Partial

1. Move Firecracker warm-hit materialization onto the shared writable-root
   volume path.
2. Use published snapshot `storage_ref` values as the source for writable child
   preparation.
3. Add richer dependency input modeling beyond the current
   workspace-plus-command-plus-key-files slice.
4. Add a user-facing changeset reuse surface if durable changesets need to be
   replayed outside the original sandbox create request.

The stage-cache flow is architecturally landed, but the full performance win
still depends on clone-capable storage instead of the default `file` driver,
and the dependency stage still needs richer lockfile/toolchain inputs.

### Remaining

1. Add richer toolchain input digests for dependency-stage keys beyond the
   current workspace-plus-command-plus-key-files slice.
2. Add lockfile parser inputs and artifact allowlists so dependency-stage keys
   can move beyond manually declared key files.
3. Broaden block-volume output semantics only after the declared block model is
   stable in real workloads.
4. Add additional ecosystems only after the first explicit dependency-stage
   flow is solid.
5. Add a user-facing `--changeset <id>` or equivalent reuse surface if durable
   changesets need explicit replay outside the original create request.
6. Add strict offline warm-cache mode and fail-closed launch checks.
7. Add garbage collection and retention policies after the key model and
   publication flow are stable.
8. Revisit cross-host distribution/export only after the local host model is
   proven worthwhile.

## Testing Plan

### Key determinism

- identical inputs produce identical keys
- identical local worktrees packaged against the same base checkout produce the
  same changeset digest
- irrelevant field ordering does not change keys
- parser version changes do change keys when parser behavior is versioned

### Publication safety

- partially written stage-cache entries are never visible as `ready`
- failed publishes do not corrupt existing entries
- concurrent publishes of the same key coalesce correctly

Current coverage exists for the implemented host-local stage-cache flow:

- warm workspace-stage hits reuse snapshot-backed sandbox creation
- runtime-base changes invalidate workspace-stage reuse
- restore failures fall back to cold bootstrap and republish
- writable-volume preparation cleans up failed clones and uses the configured
  volume-store driver
- changesetstore round-trips explicit changeset metadata and payloads, dedupes
  by stable identity, and detects payload corruption
- sandbox creation persists explicit repository changesets before applying them
- exact dependency-stage hits skip dependency bootstrap
- services-stage hits skip dependency and services bootstrap
- local changeset payloads are replayed after exact repository checkout
- dependency-stage key files resolve from the post-apply tree when a changeset
  is present
- portable dependency-stage hits refresh the checkout before execution
- portable dependency-stage hits recompute dependency key-file digests after
  refresh and skip dependency bootstrap only when they match
- portable dependency-stage mismatches discard the restored child and fall back
  to the normal workspace-stage path
- dependency and service block-volume cache keys include command, environment,
  declared input-file digests, normalized output declarations, prior block
  output identities, and service dependency output identities
- dependency and service block-volume misses run from isolated input projections
  with overlay write capture and exact full-rootfs fallback for escaped writes

### Policy enforcement

- lockfile-derived artifact allowlists block undeclared package requests
- offline warm mode fails closed when a required artifact is missing
- git repository materialization blocks until the requested commit exists in the
  mirror

### Runtime performance

- warm dependency-stage hit skips repository clone and dependency install
- warm portable dependency-stage hit skips dependency install while still doing
  an incremental checkout refresh through the gateway cache
- Firecracker warm hits avoid full rootfs file copies
- snapshot clone latency and VM boot latency are measured separately

## Open Questions

- Which ecosystem should be the first strict lockfile-enforced package cache:
  npm, pip, or another?
- Should automatic dependency-stage publication remain the default once
  retention and strict offline modes exist, or should policy be able to require
  explicit promotion?
- Should portable dependency-stage reuse be opt-in, or should it be the default
  whenever non-empty dependency key files are declared and outputs are portable?
- What retention policy should apply to large dependency-stage caches relative
  to smaller workspace-stage caches?
- For full-rootfs portable dependency stages, how much of the remaining
  portability model still matters once declared block-volume outputs cover
  repo-local state?
- Should workspace-stage identity continue to include the local checkout branch,
  or should branch stay outside reusable cache keys once checkout mode is
  modeled separately?
- Do we need a specialized optional read-only guest-visible artifact volume for
  very large immutable datasets, or can the single-rootfs model handle the
  first production slice cleanly?
- When we later revisit cross-host distribution, should exported stage caches be
  rebuilt from transport caches or exported as separate portable artifacts?
