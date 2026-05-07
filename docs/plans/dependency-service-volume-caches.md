# Dependency and Service Volume Cache Plan

**Spec reference:** `spec.md` sections 5.1.1, 5.2, 6.4
**Status:** Active
**Last reviewed:** 2026-05-07

## Summary

Dependency and service caches should be deterministic without making dependency
commands feel special. Users declare the files that define a block and the guest
paths that block naturally writes. Cleanroom owns the internal mapping from
those paths to managed cache storage.

The base design is overlay-first capture:

1. Prepare the normal checkout at the repository path, usually `/workspace`.
2. Run the dependency or service command against that normal filesystem shape.
3. Capture writes through a guest overlay.
4. Promote declared outputs into managed cache storage.
5. Restore those same guest paths on cache hit.

Declared output paths may be under `/workspace`. That is the common case for
`node_modules`, `vendor/bundle`, generated dependency indexes, and other
repo-local dependency state. The cache mechanism must not require users to move
those outputs outside the checkout.

The previous read-only input projection model was too surprising. It made the
workspace stop behaving like the workspace and forced users to rewrite commands
around Cleanroom internals. Input projection may remain useful as a strict test
primitive, but it is not the default cache execution model.

## Problem

Whole-rootfs dependency and service caches are tied to one exact checkout. They
are safe, but source-only changes miss useful dependency state even when the
dependency inputs did not change.

Portable dependency reuse tried to recover some of this by restoring an older
rootfs snapshot and refreshing the checkout. That still loses common repo-local
outputs because checkout refresh can remove directories like `node_modules`,
`vendor/bundle`, and generated files under `/workspace`.

The useful cache boundary is smaller:

- declared repository files and environment define the cache key
- declared output paths define the materialized state that can be reused
- everything else is either scratch, an escaped persistent write, or an
  undeclared input that should block portable publication when detected

## Goals

- Keep the user contract to explicit `inputs.files`, optional `env`, and
  declared `outputs.dirs` or `outputs.files`.
- Let commands run in the normal checkout, with the normal working directory and
  normal writable `/workspace` behavior.
- Allow declared outputs under `/workspace`.
- Restore cache hits at the exact same guest paths the command would have
  written.
- Use guest overlayfs write capture as the base miss path.
- Promote declared outputs from the overlay view into managed cache storage.
- Detect escaped persistent writes and skip portable publication when a block
  writes outside declared outputs.
- Keep exact full-rootfs stage caching available for unsupported backends,
  incomplete declarations, and fallback paths.
- Keep the policy and control-plane contract backend-neutral.

## Non-goals

- Replacing `content-cache` as the package-byte cache.
- Capturing live VM memory, service processes, sockets, or background daemons.
- Making arbitrary commands safely reusable without declared inputs and outputs.
- Asking users to name cache volumes, snapshots, or storage drivers.
- Requiring users to enumerate every file inside an output directory.
- Adding a user-visible cache mode for this behavior.
- Making direct output-volume mounts the semantic foundation. They are an
  optimization after overlay-first behavior is correct.

## Policy Shape

Dependency and service blocks are ordered lists. Each block has a stable name, a
command, declared inputs, optional environment, declared outputs, and optional
volatile paths for writes that are allowed but not persisted.

```yaml
sandbox:
  dependencies:
    - name: node
      command: npm ci
      inputs:
        files:
          - package.json
          - package-lock.json
          - .npmrc
      env:
        npm_config_cache: "${HOME}/.cache/npm"
      outputs:
        dirs:
          - node_modules
      volatile:
        paths:
          - "${HOME}/.cache/npm/**"

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
```

Repo-local outputs are first-class:

```yaml
sandbox:
  dependencies:
    - name: ruby
      command: bundle install
      inputs:
        files:
          - Gemfile
          - Gemfile.lock
          - .ruby-version
          - .bundle/config
      env:
        BUNDLE_PATH: "vendor/bundle"
      outputs:
        dirs:
          - vendor/bundle
```

Service prep uses the same model:

```yaml
sandbox:
  services:
    - name: postgres-data
      command: ./scripts/prepare-postgres-data.sh
      inputs:
        files:
          - compose.yaml
          - scripts/prepare-postgres-data.sh
          - db/schema.sql
          - db/seed.sql
      outputs:
        dirs:
          - /var/lib/cleanroom/services/postgres
```

### Path Expansion

`inputs.files` are repository-relative paths or glob patterns. They are resolved
from the post-checkout, post-changeset repository tree.

`outputs.dirs`, `outputs.files`, `volatile.paths`, and block `env` values
support limited guest path expansion. Expansion uses the normalized repository
destination from `repository.path`; when the policy omits it, that destination
defaults to `/workspace`.

- `~` and `~/...` expand to the Cleanroom block execution home.
- `${HOME}` and `$HOME` expand to the same home path.
- `${WORKSPACE}` and `$WORKSPACE` expand to the normalized repository
  destination path.
- Relative output and volatile paths resolve against the repository destination
  path. `node_modules` and `./node_modules` are equivalent, but examples should
  prefer `node_modules`.
- Unknown variables, empty output paths, globs in output paths, and paths that
  normalize outside their allowed root are rejected.
- Normalized outputs must be unique and non-overlapping across dependency and
  service blocks.
- A file output inside a declared output directory is rejected as redundant.
- Block `env` values remain literal strings after the limited leading path
  expansion above. Relative env values are not normalized by Cleanroom because
  each tool owns its own environment semantics.

Output paths under `/workspace` are allowed.

The normalized repository destination is part of the behavior, not a display
detail. A repository at `/workspace` with `outputs.dirs: [node_modules]` and a
repository at `/src` with the same relative declaration produce different
normalized guest output paths, output records, and cache identities.

### Volatile Writes

`volatile.paths` are explicit unpersisted write allowances. They are for logs,
pid files, package-manager noise, and other data that may be created during the
block but must not be required by later phases.

```yaml
sandbox:
  dependencies:
    - name: node
      command: npm ci
      inputs:
        files:
          - package.json
          - package-lock.json
      outputs:
        dirs:
          - node_modules
      volatile:
        paths:
          - "${HOME}/.npm/_logs/**"
          - ".cache/install-logs/**"
```

Volatile paths:

- may be absolute guest paths or repository-relative paths
- may use globs
- are not copied into managed output storage
- are not restored on cache hit
- must not overlap declared outputs
- must not be broad whole-tree ignores such as `/workspace/**`, `${WORKSPACE}/**`,
  `/**`, `**`, `/etc/**`, or `/usr/**`

Any persistent write outside declared outputs, scratch paths, and volatile paths
is still an escaped write and blocks portable publication.

## Semantics

Blocks run in policy order. A later block sees outputs restored or produced by
earlier blocks.

Each block has an independent cache identity. If the `node` block hits and the
`go-modules` block misses, Cleanroom restores `node` outputs, skips `npm ci`,
and runs only the Go module command.

The cache key includes:

- backend and runtime identity
- canonical repository reuse namespace
- compiled policy identity
- normalized repository destination path
- block stage and block name
- command digest
- normalized declared environment digest
- declared input manifest digest
- normalized output and volatile declaration digest
- prior dependency or service block output keys that this block depends on
- output volume layout version
- producer version

The declared input manifest records regular-file path, mode, and content digest
for literal files and glob matches. Missing literal inputs fail. Empty glob
matches fail. Symlinks, directories, devices, and other non-regular inputs fail
until the input manifest format explicitly supports them.

### Cache Miss

On a portable block miss, Cleanroom:

1. Starts from the normal sandbox rootfs and normal repository checkout.
2. Creates a per-block overlay root with the current rootfs as lowerdir.
3. Mounts scratch paths such as `/tmp`, `/var/tmp`, and `/run` as scratch or
   filters them from the escaped-write report.
4. Runs the command inside the overlay view with the normal block working
   directory, usually `/workspace`.
5. Scans the overlay upperdir.
6. Treats declared output writes, volatile writes, and scratch writes as allowed.
7. Treats every other persistent write as an escaped write.
8. Copies declared `outputs.dirs` and `outputs.files` from the merged overlay
   view into a temporary managed output store only after publishability checks
   pass.
9. Publishes a portable cache entry only after output metadata is durably
   persisted.
10. Materializes the declared outputs back into the real sandbox root for later
    blocks, services, and user commands.

If escaped writes are found, Cleanroom skips portable publication, discards the
overlay result, and reruns the block against the real rootfs through the exact
full-rootfs path before later phases continue. This preserves command behavior
without storing an unsafe partial cache entry.

Any managed output store created during a miss is temporary until metadata
publication succeeds. Cleanroom must destroy temporary output stores on command
failure, escaped writes, undeclared read fallback, metadata persistence failure,
context cancellation, and exact fallback. Cleanup errors should be surfaced or
recorded so leaked storage is not silent.

### Cache Hit

On a portable block hit, Cleanroom prepares the repository checkout normally,
then restores declared outputs at their normalized guest paths before skipping
the command.

Directory outputs are restored as whole directory trees. File outputs are
restored as regular files without hiding sibling files in their parent
directory.

### Baseline Output Collisions

The first implementation should only publish and restore portable directory
outputs when the output path is absent or an empty directory in the baseline
checkout. It should only publish and restore portable file outputs when the file
path is absent in the baseline checkout.

If the committed tree already contains a non-empty output directory or an output
file at the declared path, Cleanroom should skip portable volume publication for
that block and use exact full-rootfs caching. Later work can support baseline
merges by including the baseline output digest in the key, but that is not part
of the reset slice.

## Determinism Model

The user-facing determinism contract is that `inputs.files` must fully describe
the repository files that influence the block output. Cleanroom should make
that contract easy to test and hard to misuse, but it should not enforce it by
changing the command's filesystem shape.

Overlay write capture proves the block did not leave persistent effects outside
declared outputs. It does not prove the block did not read undeclared files.

Read auditing is the stronger future publish gate. When a backend can audit
workspace and mutable ambient reads, Cleanroom should compare observed reads
with the declared block state. Undeclared mutable reads should skip portable
publication and fall back to exact full-rootfs caching.

Until read auditing exists, portable publication relies on the declared-input
contract plus write-capture safety gates. That is acceptable for an initial
implementation if the documentation and stream messages make fallback decisions
clear.

### Ambient State Model

Portable blocks run with the normal checkout shape, but not with arbitrary
ambient machine state. The publishable state model is:

- block commands receive a closed environment made from Cleanroom baseline
  variables plus declared block `env`
- normalized declared environment is hashed into the block identity
- image and immutable rootfs reads are covered by backend, runtime, and image
  identity
- prior dependency and service outputs are covered by prior block output keys
- `HOME` is a controlled Cleanroom home, not a copy of arbitrary host or guest
  home state
- mutable home, global, or tool state that can affect outputs must be declared
  as `inputs.files`, declared as an output, declared as volatile, supplied by a
  modeled secret/config input, or block portable publication
- secrets must be referenced through declared secret/config identity; raw secret
  values should not be silently hashed into cache keys or copied into output
  records

Reads from immutable runtime paths such as `/usr/bin`, system libraries, and CA
bundles are acceptable because runtime identity is in the key. Reads from
mutable paths such as `${HOME}/.npmrc`, package-manager global config,
credential helpers, restored scratch state, or generated global config are not
acceptable unless they are explicitly modeled by the block contract.

## Output Storage

Start with one aggregate managed output store per block. Directory outputs use
subdirectories. File outputs use stored files.

Example:

```text
outputs:
  dirs:
    - node_modules
    - ${HOME}/.npm
  files:
    - .cache/dependency-index.json

managed block output store:
  dirs/
    0/  -> /workspace/node_modules
    1/  -> /root/.npm
  files/
    0   -> /workspace/.cache/dependency-index.json
```

The aggregate-per-block layout keeps lookup, restore, publish, and cleanup
atomic. Per-path volumes can come later if partial reuse becomes important.

## Direct Mount Optimization

Overlay-first capture is the semantic baseline. After that works, Cleanroom can
optimize misses by mounting writable output volumes at declared directory paths
before running the command.

Direct mounting is allowed only when it does not change command-visible
filesystem contents:

- the output directory is absent or empty in the baseline checkout
- the mount happens at the same guest path declared in policy
- the block still runs with overlay write capture for escaped-write detection
- file outputs are still captured from the overlay view unless a later design
  supports safe file-level promotion

If those checks fail, Cleanroom uses overlay-copy promotion or exact fallback.
Users do not choose this with policy. It is an internal optimization.

## Backend Boundary

The public policy stays backend-neutral. Backends advertise what they can safely
do:

- `sandbox.overlay_write_capture`: run a command in an overlay view and report
  upperdir writes.
- `sandbox.cache_output_volumes`: restore and snapshot managed output storage.
- `sandbox.cache_output_fast_clone`: clone output storage without copying full
  bytes.
- future `sandbox.read_audit`: report workspace and mutable ambient reads for
  publish validation.

Backends without overlay write capture or output volume support should still run
the commands normally and may use exact full-rootfs stage caches. They must not
silently claim portable file-keyed output reuse.

## Control Service Flow

For each dependency or service block:

1. Normalize inputs, environment, and outputs.
2. Resolve and hash declared inputs from the post-changeset tree.
3. Compute the block cache key.
4. Look up a ready output cache record.
5. On hit, restore declared outputs and skip the command.
6. On miss, run overlay-first capture.
7. Check baseline collisions, escaped writes, ambient-state constraints, and any
   read-audit result.
8. Promote declared outputs into temporary managed storage only when publishable.
9. Persist output records and output manifest digests.
10. Mark the managed output store durable only after metadata persistence
    succeeds.
11. Materialize outputs into the sandbox root for later phases.
12. On unsafe or unsupported cases, destroy temporary output stores and fall back
    to exact full-rootfs execution.

Service block keys include the dependency output identities they depend on. The
first implementation can make every service block depend on the complete
ordered dependency result. Later work can add explicit `depends_on`.

## Observability

Create-sandbox stream messages should describe the user-visible decision, not
the storage mechanics:

- `checking dependency cache: node`
- `restoring dependency outputs: node`
- `running dependency bootstrap: ruby`
- `publishing dependency outputs: ruby`
- `skipping portable dependency cache for ruby: undeclared writes detected`
- `skipping portable dependency cache for node: output path exists in checkout`
- `falling back to exact dependency cache: node`

Telemetry should include:

- `cleanroom.cache.stage=dependency|services`
- `cleanroom.cache.operation=lookup|restore|run|publish|fallback`
- `cleanroom.cache.result=hit|miss|published|skipped|failed`
- `cleanroom.cache.block_name=<name>`
- `cleanroom.cache.output_dir_count=<count>`
- `cleanroom.cache.output_file_count=<count>`
- `cleanroom.cache.escaped_write_count=<count>`
- `cleanroom.cache.undeclared_read_count=<count>` when read auditing exists
- `cleanroom.cache.temporary_output_cleanup=ok|failed` when cleanup runs

## Current State

Current main has useful pieces but not the intended semantics:

- policy and proto support ordered dependency and service blocks
- policy currently rejects `/workspace` outputs
- dependency and service miss execution currently uses a read-only input
  projection over the workspace
- guest overlay capture exists
- Firecracker and darwin-vz have sidecar output-volume support
- publishing and restore paths are wired around the projection-first model

The reset work should preserve the reusable backend pieces while changing the
semantic center back to normal checkout execution plus overlay-first capture.

## Delivery Strategy

### Slice 1: Contract reset

- Update spec, docs, examples, and tests so workspace-relative outputs are valid.
- Remove wording that says dependency or service commands run from a read-only
  declared-input projection.
- Document overlay-first capture as the base miss path.
- Add examples for `node_modules`, `vendor/bundle`, home cache dirs, and service
  data dirs.
- Add `volatile.paths` examples for unpersisted write allowances.

Definition of done: policy docs and examples describe the intended model
without exposing storage internals or user-visible modes.

### Slice 2: Policy and planning

- Remove policy rejection of outputs under `/workspace`.
- Keep output path normalization, uniqueness, and overlap validation.
- Add `volatile.paths` validation and escaped-write classification.
- Make output normalization repository-path-aware instead of hard-coding
  `/workspace`.
- Add runtime planning for baseline output collisions.
- Make cache keys include the normalized repository destination plus normalized
  workspace output declarations.
- Define closed block environment and controlled ambient mutable state.

Definition of done: schema validation accepts workspace outputs, but planning
can still skip portable publication when baseline collisions make restoration
unsafe.

### Slice 3: Overlay-first execution

- Stop binding the input projection over the workspace for cacheable blocks.
- Run block commands in the overlay view with the normal repository working
  directory.
- Promote declared output dirs and files from the merged overlay view.
- Materialize promoted outputs back into the real sandbox root after a
  publishable miss.
- Preserve exact fallback when escaped writes are found.
- Destroy temporary output stores on every fallback, cancellation, and publish
  failure path.

Definition of done: a dependency command can write
`node_modules`, publish that output, and leave later blocks seeing the restored
directory.

### Slice 4: Restore and reuse

- Restore declared outputs on cache hit at their exact guest paths.
- Prove a source-only commit reuses dependency and service output caches without
  rerunning block commands.
- Ensure exact full-rootfs fallback still gives later phases the command's real
  effects.

Definition of done: managed VM smoke tests show a warm hit for workspace and
home-directory outputs across a source-only commit.

### Slice 5: Direct mount optimization

- Mount writable output volumes before the command only for absent or empty
  output directories.
- Keep overlay capture active around direct mounts.
- Fall back to overlay-copy promotion when a direct mount would hide baseline
  contents.

Definition of done: common large directory outputs avoid miss-time copying
without changing command-visible behavior.

### Slice 6: Read audit publish gate

- Add backend capability for workspace and mutable ambient read auditing.
- Compare observed workspace and mutable ambient reads with declared inputs,
  modeled prior outputs, runtime identity, and declared secret/config inputs.
- Skip portable publication and fall back to exact caching on undeclared mutable
  reads.

Definition of done: a block that reads an undeclared workspace or mutable
ambient file is not published as portable cache when read auditing is available.

## Verification

Minimum coverage:

- policy validation accepts `node_modules`, `./node_modules`,
  `${WORKSPACE}/node_modules`, and `/workspace/vendor/bundle` outputs
- policy validation and cache-key tests cover `repository.path: /src`, proving
  relative outputs and `${WORKSPACE}` normalize to `/src/...` and do not reuse
  `/workspace/...` records
- policy validation rejects unknown variables, duplicate outputs, overlapping
  outputs, root output `/`, path traversal, globs in output paths, and file
  outputs inside output dirs
- policy validation accepts volatile globs and rejects volatile paths that
  overlap outputs or ignore broad whole-tree prefixes
- examples validate against the implemented schema
- cache keys change when declared inputs, command, env, output declarations,
  normalized repository destination, prior block outputs, runtime identity, or
  producer version changes
- block execution uses a closed environment and controlled `HOME`
- overlay-first execution runs with the real repository path as the working
  directory
- declared workspace directory outputs are promoted from the overlay view
- declared home directory outputs are promoted from the overlay view
- declared file outputs are promoted and restored without hiding sibling files
- escaped persistent writes skip portable publication and trigger exact fallback
- volatile writes do not skip portable publication and are not restored on hit
- baseline output collisions skip portable publication
- temporary output stores are destroyed on command failure, escaped writes,
  undeclared reads, metadata failure, cancellation, and exact fallback
- cache hits restore output dirs and files before later dependency, service, and
  execution phases
- source-only commits reuse dependency and service outputs without rerunning
  block commands
- direct mount optimization is used only for absent or empty output dirs
- read audit, when available, rejects undeclared workspace reads

## Key Learnings From Pressure-Testing

- Read-only input projection gives strong determinism but poor usability. It
  should not be the default behavior because normal dependency commands expect
  the full checkout.
- Overlay write capture is the right base for invisible caching because it lets
  commands run normally while still separating declared outputs from escaped
  writes.
- Direct output-volume mounts are valuable for performance, but only as an
  optimization. Mounting too early can hide committed files and change command
  behavior.
- Volatile paths are necessary for harmless write noise, but they must stay
  explicit and narrow so they do not become a generic ignore escape hatch.
- Declared inputs are a correctness contract until read auditing exists. Read
  auditing should become the stronger publish gate, not a user-visible mode.
- Commands can run in the normal checkout without inheriting arbitrary mutable
  state. Closed environments and controlled home/global state are required to
  keep hidden inputs from invalidating portable reuse.
- Temporary output storage must be treated as uncommitted until metadata
  persistence succeeds; otherwise escaped-write fallback can leak storage.

## Settled Decisions

- `inputs.files` plus declared outputs are the public cache contract.
- Output paths are natural guest paths, not user-managed volume identifiers.
- Workspace outputs are allowed.
- Workspace outputs should usually be written as repository-relative paths such
  as `node_modules`; `${WORKSPACE}/node_modules` and
  `/workspace/node_modules` are accepted when an absolute guest path is clearer.
- Relative workspace outputs and `${WORKSPACE}` are resolved against
  `repository.path`, not hard-coded `/workspace`.
- `volatile.paths` allow explicit unpersisted writes.
- Portable blocks use a closed environment and controlled mutable ambient state.
- Overlay-first capture is the base miss behavior.
- Direct output mounts are an internal optimization.
- Escaped persistent writes prevent portable publication.
- No user-visible cache mode is added.

## Deferred Work

- Baseline merge support for output paths that already contain committed files.
- Optional inputs and optional outputs.
- Per-path output volumes for partial reuse.
- Explicit service `depends_on`.
- Cross-repository reuse namespaces beyond the canonical repository remote.
