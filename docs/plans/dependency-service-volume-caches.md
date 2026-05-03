# Dependency and Service Volume Cache Plan

**Spec reference:** `spec.md` sections 5.1.1, 5.2, 6.4
**Status:** Proposed
**Last reviewed:** 2026-05-03

## Summary

Make dependency and service preparation reusable from deterministic file inputs
without coupling the cache entry to one exact repository checkout.

The current layered-cache plan snapshots whole root filesystems. That is still
the right fallback for exact workspace reuse, but it makes portable dependency
reuse awkward because dependency outputs can be wiped by repository refresh. The
target model here is different: declared input files produce declared output
directories and files, and Cleanroom backs those outputs with cacheable storage.

## Problem

Dependency and service preparation often has two different kinds of state:

- source files that define what should be installed or prepared
- materialized output such as toolchains, package stores, compiled extensions,
  Docker layers, database data directories, or service seed state

Today dependency and service stage cache keys are conservative because they are
built on parent workspace or dependency stage keys. That avoids unsafe reuse,
but it also means a source-only repository change can miss caches even when the
dependency or service inputs did not change.

Portable dependency reuse improves this for some cases, but it still restores a
full rootfs snapshot that contains an older checkout. Cleanroom then refreshes
the checkout and validates declared key files. This only works when useful
dependency output survives `git reset --hard` and `git clean -ffdx`, which
excludes common repo-local outputs like `node_modules`, `vendor`, generated
files, or service prep that writes under `/workspace`.

## Goals

- Key dependency and service cache entries from declared file inputs, command
  digest, environment, policy/runtime inputs, and normalized output declarations.
- Support multiple ordered dependency blocks so toolchain, language package,
  and application dependency prep can reuse independently.
- Back declared output directories with snapshot-capable volumes before commands
  run.
- Restore matching output-volume snapshots and file outputs at the same guest
  paths on cache hit.
- Run cacheable dependency and service commands from an isolated, read-only
  projection of their declared inputs rather than from the full workspace.
- Use guest overlayfs write capture, where supported, to detect persistent writes
  outside declared outputs before publishing a file-keyed cache entry.
- Scope file-keyed volume reuse to the canonical repository remote by default.
- Keep `content-cache` responsible for host-side reuse of immutable downloaded
  bytes.
- Keep exact full-rootfs stage caches available for existing behavior and for
  commands that do not fit this stricter input/output contract.
- Preserve backend-neutral policy and control-plane semantics, with backend
  capability checks for volume attachment and fast clone support.

## Non-goals

- Replacing `content-cache` with another package-byte cache.
- Capturing live VM memory, service processes, or open sockets.
- Making arbitrary shell commands safely cross-repo reusable without declared
  inputs and outputs.
- Sharing cacheable dependency or service outputs across repositories without an
  explicit trust namespace.
- Tracing arbitrary command reads in the first implementation slice.
- Requiring users to enumerate every file created inside an output directory.
- Persisting undeclared writes when a file-keyed cache hit skips the command.
- Supporting host workspace mounts.
- Supporting workspace output declarations in the first implementation slice.

## Policy Shape

The new schema should make both dependencies and services ordered lists of named
cacheable blocks. Each block declares the files that define the key and the guest
paths that should produce reusable outputs. Docker is a sandbox runtime
capability, not a service cache block, so Docker enablement moves to
`sandbox.docker.required`.

Block names must be unique within their phase, stable across policy edits, and
safe for logs and cache metadata. A conservative first rule is
`[A-Za-z0-9][A-Za-z0-9_.-]*`.

```yaml
sandbox:
  docker:
    required: true

  dependencies:
    - name: toolchains
      command: mise install
      inputs:
        files:
          - mise.toml
          - mise.lock
      outputs:
        dirs:
          - "${HOME}/.local/share/mise"
          - "${HOME}/.cache/mise"

    - name: go-modules
      command: mise exec -- go mod download
      inputs:
        files:
          - mise.toml
          - mise.lock
          - go.mod
          - go.sum
      env:
        GOMODCACHE: "${HOME}/go/pkg/mod"
        GOCACHE: "${HOME}/.cache/go-build"
      outputs:
        dirs:
          - "${HOME}/go/pkg/mod"
          - "${HOME}/.cache/go-build"

  services:
    - name: postgres
      command: |
        ./scripts/prepare-postgres-volume.sh
      inputs:
        files:
          - compose.yaml
          - .env.cleanroom
          - scripts/prepare-postgres-volume.sh
          - db/schema.sql
          - db/seed.sql
      outputs:
        dirs:
          - /var/lib/cleanroom/services/postgres
```

There is no backward-compatibility layer for the old single-object
`sandbox.dependencies` shape, the old `sandbox.services.command` and
`sandbox.services.key` shape, or `sandbox.services.docker.required`. This project
is pre-1.0, and carrying both shapes would make the cache contract harder to
explain and test. Existing policies should be migrated to the new list shape and
`sandbox.docker.required`.

### Path expansion

`inputs.files` are repository-relative paths or glob patterns. They are
resolved from the post-changeset repository tree when a changeset is present.

`outputs.dirs` and `outputs.files` support limited guest-path expansion:

- `~` and `~/...` expand to the sandbox user's home directory.
- `${HOME}` and `$HOME` expand to the same canonical home directory.
- `${WORKSPACE}` and `$WORKSPACE` expand to the Cleanroom workspace root before
  workspace-output validation runs.
- unknown variables, `~otheruser`, relative paths, empty paths, globs in output
  paths, and paths that normalize outside their allowed root are rejected.
- normalized absolute guest paths are used for cache keys and mount decisions.
- output directories and files under `/workspace` are rejected in the first
  slice.
- `outputs.dirs` are whole directory trees backed by writable cache volumes.
- `outputs.files` are individual regular files captured from overlayfs write
  capture and restored by Cleanroom before later blocks or user commands.
- declared outputs are required in the first slice. Optional outputs can be added
  later as an explicit schema feature.
- normalized output paths must be unique and non-overlapping across all
  dependency and service blocks. Parent/child directory pairs are rejected in
  the first slice.
- a file output inside a declared output directory is rejected as redundant.
- multiple file outputs may share a parent directory. Cleanroom should not use a
  single-file bind mount as the primary mechanism because atomic replacement
  patterns commonly unlink and rename files.

This must be string normalization, not shell expansion.

The canonical home directory is the `HOME` value Cleanroom supplies in the
closed block execution environment. Cleanroom must set that value before output
path normalization. Block `env` values are literal strings, except leading `~`,
`~/`, `$HOME`, `$HOME/`, `${HOME}`, and `${HOME}/` forms are expanded to the
same canonical home directory, and leading `$WORKSPACE`, `$WORKSPACE/`,
`${WORKSPACE}`, and `${WORKSPACE}/` forms are expanded to the canonical
workspace root. Other `$...` values, URLs, relative paths, empty strings, and
trailing spaces are preserved and included in the block environment digest as
provided. The first implementation should not discover a different home
directory from the image at runtime.

## Semantics

Dependency blocks run in policy order. A later dependency block sees the outputs
restored or produced by earlier blocks.

Each block has an independent cache identity. If `toolchains` hits and
`go-modules` misses, Cleanroom restores the toolchain output volumes, runs only
the Go module block, then snapshots the Go module outputs.

Cacheable block commands do not run from the live `/workspace` checkout.
Cleanroom materializes the declared `inputs.files` into a read-only input
projection that preserves repository-relative paths, then runs the command with
that projection as the working directory. Declared output directories are
writable mounts, and declared output files are captured from the overlay upperdir
after the command. The real workspace is still prepared for user code and for
exact full-rootfs fallback, but file-keyed volume-cache publication must not
depend on undeclared workspace reads.

If a dependency or service command needs files that are not in its declared
input set, the policy should declare those files or globs. If the command needs
the whole repository and cannot be reduced to a declared input set, it should use
exact full-rootfs stage caching rather than the new volume-cache block shape.

Input projection must not provide symlink escape hatches back into the live
workspace or arbitrary runtime paths. The first slice should reject projected
input symlinks with absolute targets or `..` traversal outside the projection
unless the resolved target is also part of the declared input set and can be
materialized inside the projection.

Service blocks run after dependency blocks. A service block key includes the
selected dependency output identities it depends on. The first implementation
can make every service block depend on the complete ordered dependency result;
later work can add explicit `depends_on` if needed.

Directory outputs are backed before the command runs. On a miss, Cleanroom
creates a writable output volume, maps each `outputs.dirs` path into that
volume, runs the command, and snapshots the output volume. On a hit, Cleanroom
clones the output snapshot and maps it into the guest at the same normalized
directory paths before skipping the command.

File outputs are captured from the command's overlayfs upperdir. Cleanroom does
not bind-mount individual files and should not hide an existing parent directory
behind an empty capture directory. On a miss, after the command exits, Cleanroom
copies each declared `outputs.files` path from the overlay view into the block's
output store and materializes the same file into the sandbox root for later
blocks. On a hit, Cleanroom restores declared file outputs into the sandbox root
before later blocks or user commands run.

Parent directories for directory outputs may be created by Cleanroom. The
directory output itself must either be absent or be an empty directory before the
cache volume is mapped in the first implementation. Rejecting
pre-existing non-empty directory paths keeps the initial semantics simple and
avoids hiding image-provided files behind a cache volume.

The command must not leave persistent state outside declared outputs. Where the
backend supports guest overlayfs write capture, Cleanroom should enforce this by
running the command against an overlay root and inspecting the upperdir after the
command. If escaped persistent writes are found, Cleanroom must not publish the
file-keyed volume cache entry.

Escaped writes are a publication gate, not the determinism mechanism. The
determinism mechanism is the isolated input projection plus declared output
mounts and captured files. The publication gate prevents Cleanroom from caching
a subset of command effects under a key that would later skip the command.

### Reuse namespace

The initial reuse namespace is the canonical repository remote URL. That gives
cross-commit reuse inside one repository without allowing arbitrary repositories
to share dependency or service outputs just because their declared inputs match.

The namespace should be an explicit cache-key component even if it is not exposed
in the first public schema. A future policy field may allow broader sharing, but
only by naming a trust namespace deliberately.

Local commit bundles and changesets still resolve against the same canonical
repository remote. Their effects are included through the post-apply input
manifest rather than by changing the reuse namespace.

### Environment and network determinism

Block commands should receive a closed environment: Cleanroom's deterministic
baseline variables plus the block's declared `env`. Host process environment
values must not leak into file-keyed volume-cache commands unless they are
explicitly declared and hashed.

`dependency_env_digest` and `service_env_digest` include declared block env,
Cleanroom-supplied cache path variables, `HOME`, working directory, and any
other baseline values that can affect command behavior.

The compiled policy hash constrains network access, but it does not make mutable
upstream responses deterministic. File-keyed volume cache is only safe for
dependency or service commands whose external inputs are pinned by declared
files, resolved through immutable/cache-backed artifacts, or otherwise included
in the input manifest. Commands that resolve `latest` style upstream state
should stay on exact full-rootfs caching until a resolver can record the
resolved artifact identity.

### Overlay write capture

Overlay write capture is the preferred cheap mechanism for detecting undeclared
persistent writes. It is a guest execution feature, not a host volume-driver
feature.

For a cacheable block miss, the runner should:

1. create a guest mount namespace for the block
2. mount the current rootfs as an overlay lowerdir
3. create per-block `upperdir`, `workdir`, and merged root directories
4. mount scratch paths such as `/tmp`, `/run`, and `/var/tmp` as tmpfs or filter
   them from the escaped-write report
5. mount the read-only input projection at the command working directory
6. mount each `outputs.dirs` mapping as writable inside the merged root
7. run the command inside the merged root with `chroot`, `pivot_root`, or an
   equivalent backend runner primitive
8. inspect the upperdir after the command exits

The upperdir is the path-shaped diff for everything that was not sent to a
declared directory output or scratch mount. Cleanroom should subtract its own
setup baseline, such as mountpoint directories it created before the command,
before reporting escaped writes.

Declared file outputs are allowed entries in the upperdir. Cleanroom copies
their final contents into the output store and restores them into the sandbox
root after the overlay run. Ancestor directories created only to reach declared
file outputs are also allowed. Any other persistent upperdir entry is an escaped
write.

If escaped writes are found, the first implementation should warn, skip
file-keyed cache publication, restart from the pre-block state, and rerun the
block through the exact full-rootfs path. It must not discard the upperdir and
continue as if the block succeeded normally, because later commands would see a
sandbox that differs from the command's real effects.

Once the feature is stable, escaped writes should become hard failures for
cacheable blocks. The first slice uses exact fallback to keep existing workflows
moving while users learn the declaration contract.

A local `darwin-vz` experiment on 2026-05-02 validated the basic guest
mechanism: the managed Linux guest exposes overlayfs, root commands can mount an
overlay with `/` as the lowerdir, writes and whiteout deletes appear in the
upperdir, and bind-mounted declared output directories do not appear in the
upperdir except for setup-created mountpoints. That proves the mechanism is
viable, but not that every backend can attach reusable output volumes yet.

## Cache Keys

Add cache-key helpers for dependency and service volume outputs in
`internal/cachekey/cachekey.go`.

Dependency block key:

```text
dependency_volume_key = H(
  backend,
  runtime_key,
  reuse_namespace,
  compiled_policy_hash,
  block_name,
  dependency_command_digest,
  dependency_env_digest,
  dependency_input_manifest_digest,
  normalized_outputs_digest,
  prior_dependency_output_keys_digest,
  output_volume_layout_version,
  producer_version
)
```

Service block key:

```text
service_volume_key = H(
  backend,
  runtime_key,
  reuse_namespace,
  compiled_policy_hash,
  block_name,
  service_command_digest,
  service_env_digest,
  service_input_manifest_digest,
  normalized_outputs_digest,
  dependency_output_keys_digest,
  prior_service_output_keys_digest,
  output_volume_layout_version,
  producer_version
)
```

Input manifests should record regular-file path, mode, and content digest, and
reject directories, symlinks, devices, and other non-regular inputs. They should
be sorted before hashing. Missing required literal paths should fail. Glob
matches should be sorted and should fail if the pattern is invalid, matches no
files, or matches non-regular files. Optional inputs can be added later as an
explicit schema feature; the first implementation should not hash an empty glob
as an empty input set because that is usually a typo and creates an overbroad
cache key. Input projection validation must reject symlink escapes before
running the block command.

The normalized output declaration is part of the key because changing directory
mount destinations or file-output restore paths can change command behavior even
when file inputs are the same.

## Volume Layout

Start with one aggregate output store per block. Directory outputs use mounted
subdirectories. File outputs use stored files that Cleanroom copies into the
sandbox root on cache hit or after a successful miss.

Example:

```text
outputs.dirs:
  - ${HOME}/.local/share/mise
  - ${HOME}/go/pkg/mod
outputs.files:
  - ${HOME}/.cache/tool/index.json

dependency block volume:
  dirs/
    home-cleanroom-.local-share-mise/
    home-cleanroom-go-pkg-mod/
  files/
    home-cleanroom-.cache-tool-index.json

guest view:
  /home/cleanroom/.local/share/mise -> mounted/bound mapped output dir
  /home/cleanroom/go/pkg/mod        -> mounted/bound mapped output dir
  /home/cleanroom/.cache/tool/index.json restored as a file
```

The aggregate-per-block layout makes lookup, clone, publish, and rollback
atomic. Per-path volumes can come later if partial reuse becomes important.
Directory outputs may need real block devices or bind mounts for speed. File
outputs are expected to be small enough that copying them is acceptable and
avoids brittle single-file mounts.

## Backend Boundary

Add backend capabilities and request fields for cache output volumes rather than
teaching the control service backend internals.

Suggested capability names:

- `sandbox.cache_output_volumes`
- `sandbox.cache_output_fast_clone`
- `sandbox.overlay_write_capture`

The first implementation should not rely on hotplug. All directory output
volumes needed for dependency and service blocks must be resolved before VM
launch, then passed to provisioning as sidecar volume specs. Cache hits clone
existing output snapshots; misses create empty writable volumes. The VM boots
once with the rootfs plus all cache output volumes attached. File outputs can be
stored in the same aggregate output store, but they are restored by file copy
rather than by mounting a single file.

Suggested backend types in `internal/backend/backend.go`:

```go
type CacheOutputVolumeSpec struct {
  Stage string
  BlockName string
  VolumeID string
  SourceSnapshotRef string
  DirMappings []CacheOutputDirMapping
  FileMappings []CacheOutputFileMapping
}

type CacheOutputDirMapping struct {
  GuestPath string
  VolumeSubpath string
}

type CacheOutputFileMapping struct {
  GuestPath string
  VolumePath string
}

type ProvisionRequest struct {
  SandboxID string
  Policy *policy.CompiledPolicy
  FirecrackerConfig
  CacheOutputVolumes []CacheOutputVolumeSpec
}

type ProvisionFromSnapshotRequest struct {
  SandboxID string
  SnapshotID string
  StorageRef string
  Policy *policy.CompiledPolicy
  FirecrackerConfig
  CacheOutputVolumes []CacheOutputVolumeSpec
}

type CacheOutputVolumeAdapter interface {
  Adapter
  SnapshotCacheOutputVolumes(context.Context, SnapshotCacheOutputVolumesRequest) (*SnapshotCacheOutputVolumesResult, error)
  DestroyCacheOutputVolumes(context.Context, DestroyCacheOutputVolumesRequest) error
}
```

The provisioning request carries enough information for the backend to create or
clone sidecar volumes before launch and mount directory outputs at the
normalized guest paths. Snapshot and destroy requests should return enough
metadata for the control service to persist cache records and clean up failed
attempts. File mappings are used by the guest runner to extract and restore
individual file outputs from the aggregate output store.

Firecracker is the first target. It already has a volume-store abstraction in
`internal/volumestore/store.go` and ZFS-backed clone support in
`internal/backend/firecracker/backend.go`. The first Firecracker slice can
extend the launch drives list with one additional block device per aggregate
output volume, then run guest-side mount or bind setup before the
dependency/service command. It should also add the guest overlay write-capture
runner for missed cacheable blocks. Firecracker hotplug is out of scope for the
first slice.

Darwin-VZ should initially report `sandbox.cache_output_volumes` as unsupported
unless the backend grows additional disk attach and guest mount support. Its
current docs state that `/workspace` lives on the rootfs, and current launch code
prepares a single writable rootfs image. It may still report
`sandbox.overlay_write_capture` independently once the guest runner has a
supported mount-namespace path; the local experiment showed the guest kernel and
root execution environment are capable of the basic overlayfs operations.

Backends without `sandbox.cache_output_volumes` should still run the new block
schema with the same isolated input-projection semantics, using ordinary rootfs
directories at the normalized output directories and ordinary rootfs files at
the normalized output file paths instead of sidecar volumes. They do not publish
volume caches, but they may still publish exact full-rootfs stage caches after
the projected block commands run. If they support
`sandbox.overlay_write_capture`, they can still report escaped writes for
diagnostics. This keeps command behavior stable across backends while allowing
fast volume restore only where supported.

## Metadata Store

The existing cache metadata model in `internal/cachestore/store.go` is centered
on one cache key and one storage ref. Volume caches need one cache entry with
one or more output volume refs.

Add either:

- a new `cache_volume_outputs` table keyed by stage, cache key, output index, and
  normalized path, or
- a JSON output manifest field if the store remains small and SQLite-only.

Prefer a normalized table if garbage collection and per-output observability are
expected soon. Each output record should include:

- output kind: directory or file
- normalized guest path
- volume subpath
- storage driver
- storage ref
- snapshot ref when different from storage ref
- output manifest digest
- created time and last-used time inherited from the parent cache record

Cache records should still carry policy hash, backend, producer version, input
manifest digest, normalized outputs digest, command digest, and parent dependency
or service output keys.

## Control Service Changes

Expected files:

- `internal/policy/policy.go`
- `proto/cleanroom/v1/control.proto`
- `internal/cachekey/cachekey.go`
- `internal/cachestore/store.go`
- `internal/controlservice/dependency_stage.go`
- `internal/controlservice/services_stage.go`
- `internal/controlservice/service.go`
- `internal/controlservice/cache_observability.go`
- `internal/controlservice/bootstrap_runner.go`

Policy loading should make the new schema canonical immediately:

- `sandbox.dependencies` is an ordered list of dependency blocks
- `sandbox.services` is an ordered list of service blocks
- `sandbox.docker.required` is the Docker runtime requirement
- the old single-object `sandbox.dependencies` shape is rejected
- the old `sandbox.services.command` and `sandbox.services.key` shape is rejected
- the old `sandbox.services.docker.required` location is rejected

The control service should build all dependency and service block plans before
sandbox provisioning or snapshot restore. For each block it should:

1. compute normalized input and output manifests
2. compute the block cache key
3. look up a ready cache record
4. add a hit or empty output-volume spec to the provision request
5. provision or restore the sandbox with all output volumes already attached
6. materialize read-only input projections for missed blocks
7. run only missed block commands through overlay write capture when supported
8. extract and restore declared file outputs for missed blocks
9. rerun the block through exact full-rootfs fallback when escaped writes are
   found before later phases
10. snapshot output volumes for missed blocks and persist metadata

Service block planning follows the same pre-launch shape, but service commands
run after dependency commands and include dependency output keys in their cache
identity.

## Escaped Write Handling

Workspace cleanliness alone is not enough because a dependency or service block
can write to `/etc`, `/usr/local`, `$HOME`, or another persistent rootfs path.
The publish gate should inspect the overlay upperdir, not only `/workspace`.

For dependency and service volume blocks:

- writes under `outputs.dirs` go to mounted output directories
- declared `outputs.files` are copied from the overlay upperdir into the output
  store and then materialized back into the sandbox root
- scratch writes under tmpfs or explicitly ignored scratch paths are not
  published and are not escaped writes
- any other persistent upperdir entry is an escaped write

Escaped writes prevent file-keyed volume cache publication. In the first
implementation, Cleanroom should warn and rerun the block through exact
full-rootfs fallback so the sandbox still contains the command's real effects.
After the feature is stable, cacheable blocks should fail hard on escaped writes.
The user schema should still avoid a confusing `mode` field.

## Observability

Extend cache telemetry without replacing existing stage names. Add attributes
for block name and volume-cache operation:

- `cleanroom.cache.stage=dependency|services`
- `cleanroom.cache.operation=lookup|prepare_volume|restore|publish|invalidate`
- `cleanroom.cache.result=hit|miss|restored|published|fallback|failed`
- `cleanroom.cache.block_name=<name>`
- `cleanroom.cache.output_dir_count=<count>`
- `cleanroom.cache.output_file_count=<count>`
- `cleanroom.cache.escaped_write_count=<count>`

Create-sandbox stream messages should stay user-readable:

- `checking dependency cache: toolchains`
- `restoring dependency outputs: toolchains`
- `running dependency bootstrap: go-modules`
- `skipping portable dependency cache for go-modules: undeclared writes detected`
- `publishing service outputs: postgres`

## Implementation Order

1. Add schema types for `sandbox.docker.required`, dependency blocks, and service
   blocks, including `outputs.dirs` and `outputs.files`, in
   `internal/policy/policy.go` and `proto/cleanroom/v1/control.proto`.
2. Add path expansion and validation helpers for `${HOME}`, `$HOME`, `~`, and
   `${WORKSPACE}`.
3. Add deterministic input-manifest hashing for literal files and globs.
4. Add dependency and service volume cache-key helpers in
   `internal/cachekey/cachekey.go`.
5. Add reuse namespace handling, defaulting to canonical repository remote.
6. Extend cache metadata to record output volume refs and manifests.
7. Add backend capability flags and provision request fields for cache output
   volumes and overlay write capture in `internal/backend/backend.go`.
8. Add a guest overlay write-capture runner that can execute one missed block
   from an input projection, mount declared directory outputs, extract declared
   file outputs, and report escaped writes.
9. Implement Firecracker pre-launch output-volume preparation, mount setup,
   snapshotting, clone restore, and cleanup.
10. Wire dependency block planning and lookup into
   `internal/controlservice/dependency_stage.go` and `service.go`.
11. Wire service block planning and lookup into
   `internal/controlservice/services_stage.go` and `service.go`.
12. Add isolated input projection materialization and escaped-write exact
    fallback handling.
13. Update observability events, stream phases, docs, and examples.
14. Add end-to-end tests that prove a source-only commit reuses dependency and
    service output volumes without rerunning their commands.

### Phase 1 PR Status

The first implementation PR covers the contract and plumbing layer, not the
runtime sidecar-volume execution path.

Completed in phase 1:

- policy schema and proto support for ordered dependency and service blocks
- `sandbox.docker.required` as the Docker runtime requirement
- validation for block names, commands, input files, `outputs.dirs`,
  `outputs.files`, `${HOME}`, `$HOME`, `~`, `${WORKSPACE}`, duplicate paths,
  overlapping paths, and workspace output rejection
- rejection of the old YAML object forms for `sandbox.dependencies`,
  `sandbox.services.command`, `sandbox.services.key`, and
  `sandbox.services.docker.required`
- deterministic input-manifest helper package for literal regular files,
  regular-file globs, file contents, and modes, rejecting non-regular inputs
- glob-aware dependency key-file hashing for the existing stage-cache path,
  including changeset-aware resolution
- dependency and service output-volume cache-key helper APIs
- reuse namespace helper, defaulting to canonical repository remote when no
  explicit namespace is provided
- cache metadata fields for command, env, normalized outputs, output manifests,
  and output records
- backend capability flags and provision request fields for cache output volumes
  and overlay write capture
- README, spec, API docs, and example policies updated to the new schema
- tests for schema validation, cache keys, input manifests, cache metadata,
  glob expansion, generated proto round trips, CLI Docker policy creation, and
  existing dependency/services stage-cache behavior

Current runtime behavior after phase 1:

- dependency and service blocks are compiled into the existing aggregate
  dependency/services stage bootstrap command
- existing full-rootfs dependency and services stage caches continue to work
- declared outputs are validated and represented in policy/proto/cache metadata,
  but they are not yet restored or published as independent output volumes
- overlayfs write capture is represented as a backend capability contract, but
  no backend runner enforces escaped-write detection yet

Remaining work:

- extend dependency block-volume runtime wiring from lookup-only planning to
  output-volume preparation, restore, execution, and publication
- extend service block-volume runtime wiring from lookup-only planning to
  output-volume preparation, restore, execution, and publication
- materialize isolated declared-input projections before running cacheable
  blocks
- implement the guest overlay write-capture runner
- prepare output volumes before VM launch, mount declared `outputs.dirs`, and
  copy declared `outputs.files`
- publish and restore output-volume snapshots and output manifests
- implement escaped-write warning plus exact full-rootfs fallback
- add observability events for per-block lookup, restore, execution, publish,
  and fallback decisions
- add end-to-end tests proving source-only commits reuse dependency and service
  output volumes without rerunning block commands

Steps 8 and 9 remain the main backend slices. Steps 10 and 11 should now build
on the phase 1 request fields and metadata, with stub-adapter tests before the
Firecracker implementation.

### Phase 2 PR Status

The second implementation PR starts the runtime-control layer without changing
the sandbox execution path yet. Existing dependency and service bootstraps still
run through the aggregate stage cache paths, even when block-volume records hit.

Completed in phase 2:

- dependency block-volume planner computes one cache key per ordered dependency
  block from backend, runtime key, reuse namespace, policy hash, command digest,
  env digest, input manifest digest, normalized output declarations, and prior
  dependency output keys
- dependency block-volume cache lookup marks per-block hits and misses using the
  cache metadata fields introduced in phase 1
- dependency and service block-volume keys use strict regular-file input
  manifests, rejecting symlinks, directories, gitlinks, missing paths, and
  deleted paths before file-keyed cache reuse
- dependency and service block-volume hits validate cached output records against
  the declared output kind/path set before treating metadata as restorable
- runtime fallback decision requires backend support for cache output volumes and
  overlay write capture before the future block-volume path can be selected
- sandbox creation now evaluates dependency block-volume lookup after exact and
  portable dependency-stage cache restore fail or miss, and before workspace
  cache restore or fresh provisioning
- unsupported backends and missing cache metadata stores fall back to the
  aggregate dependency stage path before doing per-block key resolution
- dependency block-volume lookup observability records block counts, hit counts,
  miss counts, and logs fallback and lookup summaries
- tests cover ordered keying, prior-block invalidation, partial cache hits, and
  fallback when backend capabilities are missing
- tests cover strict input validation and output-record mismatch rejection before
  block-volume records are treated as hits
- `CreateSandbox` tests cover unsupported backend fallback, missing store
  fallback, partial dependency block-volume hits, and all-hit lookup behavior
  while confirming the aggregate dependency bootstrap still runs
- service block-volume planner computes one cache key per ordered service block
  from backend, runtime key, reuse namespace, policy hash, command digest, env
  digest, input manifest digest, normalized output declarations, ordered
  dependency output keys, and prior service output keys
- sandbox creation now evaluates service block-volume lookup after services
  stage restore misses, including after dependency-stage cache restores, once
  dependency block-volume planning is available for policies with dependency
  blocks
- service block-volume lookup observability records block counts, hit counts,
  miss counts, and logs fallback and lookup summaries
- tests cover service keying, dependency-output invalidation, prior-service
  invalidation, partial service cache hits, unsupported backend fallback, missing
  store fallback, partial `CreateSandbox` hits, and all-hit lookup behavior while
  confirming the aggregate services bootstrap still runs
- regression coverage proves service block-volume lookup still runs when the
  aggregate services stage cache misses and an aggregate dependency stage cache
  restores

Follow-up runtime work after phase 2:

- run missed dependency blocks from isolated input projections
- restore hit output records before later dependency blocks run
- publish output records after successful missed blocks
- run missed service blocks from isolated input projections
- restore hit service output records before later service blocks run
- publish service output records after successful missed blocks

### Phase 3 PR Status

The third implementation PR continues the runtime-control layer by turning
block-volume lookup results into backend launch-time volume specs. It still does
not make any backend attach, mount, restore, execute, or snapshot those volumes;
aggregate dependency and service bootstrap commands continue to run.

Completed in phase 3:

- control service converts dependency and service block-volume plans into
  `backend.CacheOutputVolumeSpec` values with stable volume IDs, declared
  directory mappings, declared file mappings, and cache-hit source storage refs
- cache-hit output records must describe one aggregate output volume per block
  before the record can be consumed as a launch spec
- service block-volume lookup returns its computed plan to the caller instead
  of being logging-only
- sandbox creation now computes dependency and service block-volume plans after
  a services-stage cache miss and before dependency-stage or workspace-stage
  restores, so later launch requests can carry all needed output-volume specs
- dependency-stage and portable dependency-stage restores receive service
  output-volume specs only, because dependency outputs are already present in
  the restored rootfs and should not be shadowed by dependency sidecar mounts
- workspace-stage restores and cold provisions receive dependency plus service
  output-volume specs because those phases still need to run dependency and
  service bootstrap work
- tests cover spec construction for cache hits and misses, cold provision
  request wiring, and dependency-stage restore request wiring

Remaining phase 3 work:

- mount declared `outputs.dirs` and restore declared `outputs.files` inside the
  guest before block commands run
- run missed dependency and service blocks from isolated input projections
- restore hit output records before later blocks run
- snapshot and publish output records after successful missed blocks
- keep aggregate exact full-rootfs stage caches as the fallback path until
  per-block execution is complete

### Phase 4 PR Status

The fourth implementation PR starts the backend side of the runtime path for
Firecracker. It consumes the launch-time `backend.CacheOutputVolumeSpec` list
and prepares writable sidecar ext4 volumes before the VM starts. It still does
not mount those volumes in the guest, restore file outputs, run block commands,
capture writes, or publish block output snapshots; aggregate dependency and
service bootstrap commands remain the runtime fallback.

Completed in phase 4:

- Firecracker advertises `sandbox.cache_output_volumes` but intentionally does
  not advertise `sandbox.overlay_write_capture` until the guest execution path
  exists
- cold provisions and snapshot restores forward cache output volume specs into
  Firecracker launch
- Firecracker validates cache output volume specs before launch, rejects
  malformed specs, and rejects duplicate runtime volume IDs
- cache hits prepare writable volumes by cloning or copying the recorded source
  storage ref
- cache misses create an empty ext4 source image and prepare a writable output
  volume from it
- prepared output volumes are attached as non-root, writable Firecracker block
  devices alongside the rootfs
- output volume cleanup is included in all launch failure and sandbox cleanup
  paths
- tests cover capability reporting, launch request forwarding, hit and miss
  volume preparation, malformed specs, and cleanup on partial prepare failure

Remaining runtime work after phase 4:

- run missed dependency and service blocks from isolated input projections
- inspect overlay write capture results and warn/fallback on escaped writes
- snapshot and publish output records after successful missed blocks
- advertise `sandbox.overlay_write_capture` once escaped-write detection and
  isolated block execution are wired through
- decide whether output volume sizing stays internal or becomes policy-visible
  after real workloads show the right default

### Phase 5 PR Status

The fifth implementation PR wires the prepared Firecracker output volumes into
the guest execution protocol. It still keeps the block-volume runtime inactive
because Firecracker does not advertise `sandbox.overlay_write_capture` yet, and
it does not run per-block dependency or service commands.

Completed in phase 5:

- Firecracker derives a deterministic guest mount plan from prepared output
  volumes, using `/dev/vdb`, `/dev/vdc`, and later virtio block device names in
  the same order as the non-root drives
- sandbox executions forward the mount plan to the Linux guest agent through
  the internal vsock exec request
- the guest agent mounts each cache output ext4 device at a Cleanroom-managed
  path under `/run/cleanroom/cache-output-volumes`
- declared `outputs.dirs` are created inside the output volume and bind-mounted
  at the declared guest path before the command runs
- cache hits require declared directory output subpaths to already exist inside
  the restored output volume rather than creating empty replacements
- declared directory output targets must be absent or empty directories before
  the bind mount, so image-provided files are not silently hidden
- declared `outputs.files` are restored by copying regular files out of the
  output volume on cache hits, and missing file outputs are skipped on misses
- the guest agent rejects changed mount plans after the first setup in a VM, so
  repeated exec requests do not clobber restored file outputs
- tests cover mount-plan construction, request forwarding, guest mount actions,
  path validation, non-empty directory rejection, file restoration, and changed
  plan rejection

Remaining runtime work after phase 5:

- run missed dependency and service blocks from isolated input projections
- inspect overlay write capture results and warn/fallback on escaped writes
- snapshot and publish output records after successful missed blocks
- materialize captured file outputs into both the output volume and sandbox root
  after a missed block run
- advertise `sandbox.overlay_write_capture` once escaped-write detection and
  isolated block execution are wired through
- decide whether output volume sizing stays internal or becomes policy-visible
  after real workloads show the right default

## Tests

Minimum coverage:

- policy validation accepts ordered dependency/service blocks
- policy validation accepts `sandbox.docker.required` as the Docker runtime
  requirement
- policy validation rejects the old single-object `sandbox.dependencies` shape
- policy validation rejects the old `sandbox.services.command` and
  `sandbox.services.key` shape
- policy validation rejects the old `sandbox.services.docker.required` location
- policy validation rejects duplicate block names
- policy validation rejects invalid output dir and file paths, relative paths, unknown
  variables, `~otheruser`, and `/workspace` outputs
- policy validation rejects duplicate or overlapping normalized output directories
- policy validation rejects file outputs inside directory outputs
- path normalization makes `~/go/pkg/mod` and `${HOME}/go/pkg/mod` equivalent
  and rejects `${WORKSPACE}` outputs after expansion
- input manifests are deterministic across file order and glob order
- input manifest validation rejects empty glob matches
- input manifest validation rejects directories, symlinks, and other
  non-regular files
- input projection rejects symlinks that escape the declared input set
- cache keys change when inputs, env, command, output declarations, runtime key, or
  prior block output keys change
- cache keys change when the reuse namespace changes
- cache keys change when Cleanroom baseline command environment changes
- cacheable block commands run from an isolated declared-input projection, not
  from `/workspace`
- dependency block cache hit restores output volumes and skips the command
- file output cache hit restores the file without hiding sibling files in the
  parent directory
- partial dependency hit runs only missing later blocks
- service block key changes when dependency output keys change
- overlay write capture reports created, modified, deleted, and renamed paths in
  the upperdir
- declared directory output writes do not appear as escaped writes
- declared file outputs are extracted from the upperdir and restored into the
  sandbox root
- persistent writes outside declared outputs prevent portable volume cache
  publication and trigger exact fallback before later phases continue
- Firecracker launch includes cache output block devices before the VM starts
- backend cleanup runs when command execution, snapshot publication, or metadata
  persistence fails
- Firecracker ZFS path clones output volumes rather than copying ext4 bytes

## Settled Decisions

- Empty globs fail. Optional inputs can be added later with explicit schema.
- The new list schema replaces the old dependency object directly.
- `sandbox.services` is an ordered service block list immediately.
- Docker enablement moves to `sandbox.docker.required`.
- `${HOME}` is the value Cleanroom sets in the closed block execution
  environment and hashes into the block key.
- Escaped writes warn and trigger exact full-rootfs fallback in the first
  implementation.
- Escaped writes should become hard failures for cacheable blocks once the
  feature is stable.
