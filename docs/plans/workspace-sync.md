# Workspace Copy and Export Plan

**Spec reference:** `spec.md` sections 5.1.1, 5.4, 5.4.2
**Related plan:** `docs/plans/sandbox-transfer-invariants.md`
**Status:** Proposed
**Last reviewed:** 2026-04-27

## Summary

Replace the current `--include-local-changes` UX with a clearer `--copy` flag
and add explicit workspace copy/export primitives for the common "edit
locally, run in Cleanroom, bring changes back" workflow.

In this plan, "workspace" means the project tree inside the cleanroom,
normally `/workspace`. It does not mean the full sandbox filesystem, and it
does not create a host mount.

The user-facing workspace vocabulary is:

- `cleanroom workspace copy`
- `cleanroom workspace export`
- `cleanroom workspace diff`

The direction is named by the verb:

- `workspace copy` mirrors the included local workspace file set into the
  cleanroom workspace.
- `workspace export` mirrors the included cleanroom workspace file set back
  into the caller's local workspace.
- `workspace diff` shows how the cleanroom workspace differs from its recorded
  cleanroom baseline.

Mirror semantics include deletes inside the workspace file set. The safety
boundary is not "never delete"; it is "be explicit about direction, honor
ignored files, offer dry runs, and refuse to overwrite or delete local paths
that changed independently."

Cleanroom already has one-off sandbox transfer primitives for explicit file
movement:

```sh
cleanroom cp ./fixture.json <sandbox-id>:/tmp/fixture.json
cleanroom cp <sandbox-id>:/tmp/result.json ./result.json
```

Workspace copy/export should reuse existing substrates rather than redefine
low-level copy semantics. For Git-backed source workspaces, copy and export
should use repository changesets as the transport. For non-Git source
workspaces, they should use raw workspace transfer over the file/path/archive
layer. That is an implementation choice; the user-facing behavior should be
the same.

The higher-level automation should stay small and literal:

```sh
cleanroom exec --copy -- npm test
cleanroom exec --export-on-exit -- npm run fix
cleanroom exec --sync -- npm run generate
```

Top-level flags:

- `--copy` copies local workspace changes into `/workspace` before running.
  It is automation over the same operation as `cleanroom workspace copy`.
- `--export-on-exit` exports the included `/workspace` file set back into the
  local workspace after the command or console session exits.
- `--sync` is equivalent to `--copy --export-on-exit`.

## Goals

- Make local development workflows feel direct: edits made locally appear in
  `/workspace`, and useful changes made in `/workspace` can come back.
- Keep CI and hermetic workflows explicit and available.
- Keep `/workspace` copy/export behavior as command primitives that can be
  used manually and by top-level automation.
- Keep ordinary `exec`, `console`, and `create` startup fast by avoiding
  implicit whole-tree copy operations.
- Replace `--include-local-changes` with `--copy` instead of carrying both
  names long term.
- Exclude ignored dependency/cache/runtime directories from copy/export by
  default, so a Linux `node_modules/` never gets exported back to a macOS
  checkout unless the user deliberately asks for that exact path.
- Avoid host filesystem mounts and backend-specific file sharing assumptions.
- Make export safe by default: no silent overwrite of unrelated local changes,
  a dry-run path for previewing local effects, and clear conflict reporting.
- Preserve the existing repo-aware checkout model for clean committed inputs.
- Reuse the sandbox file/path/archive transfer layer for payload movement.
- Keep `--copy` and `workspace copy` behavior identical; only the transport
  should differ between Git and non-Git sources.

## Non-goals

- General-purpose arbitrary path copy across the whole sandbox filesystem; that
  belongs to `cleanroom copy` / `cleanroom cp`.
- Bidirectional conflict resolution or a full file synchronization daemon.
- Making workspace copy/export the default behavior for top-level commands.
- Making `cleanroom sandbox create` infer repository policy or local workspace
  state.
- Exporting ignored workspace artifacts by default.
- Replacing artifact upload/download APIs for non-workspace build outputs.
- Requiring the guest image to provide the `rsync` binary.

## Command Model

### Existing one-off copy primitive

The existing `cleanroom cp` command is for direct file movement:

```sh
cleanroom cp <local-file> <sandbox-id>:/absolute/path
cleanroom cp <sandbox-id>:/absolute/path <local-file-or-directory>
```

It is intentionally not a workspace lifecycle command. Current behavior is
scoped to one local file to one sandbox path, or one sandbox file to one local
path. Recursive local directory upload, workspace baselines, conflict
detection, dry-run export planning, and `/workspace` mirror semantics are out
of scope for `cp`.

Workspace commands should use a shared workspace planner with
workspace-specific safety semantics. The planner selects the transport from
the source workspace:

- Git source: repository changeset.
- Non-Git source: raw workspace transfer using the backend-neutral
  file/path/archive layer.

Top-level `--copy` must call the same copy operation as
`cleanroom workspace copy`; it is not a separate changeset-only mode.

### `cleanroom workspace copy`

Copy the caller's local workspace into the cleanroom workspace.

```sh
cleanroom workspace copy <sandbox-id>
cleanroom workspace copy --dry-run <sandbox-id>
```

Default behavior:

- source: the caller's local repository/workspace root
- destination: the sandbox workspace root, normally `/workspace`
- mirror local additions, modifications, and deletes from the included
  workspace file set into the cleanroom workspace
- skip `.git/`
- if the source is a Git worktree, package and apply a repository changeset
- if the source is not a Git worktree, use raw workspace transfer
- honor Git ignore rules by default for Git-backed source workspaces
- record the local binding and manifest needed for later export conflict
  checks

`workspace copy --dry-run` reports the planned remote writes and deletes
without applying them.

Because the cleanroom side is disposable, copy can use mirror semantics inside
the included workspace file set. `workspace copy` and `--copy` should share the
same planner and produce the same cleanroom workspace contents for the same
source.

### `cleanroom workspace export`

Export cleanroom workspace changes back to the caller's local workspace.

```sh
cleanroom workspace export <sandbox-id>
cleanroom workspace export --dry-run <sandbox-id>
```

Default behavior:

- source: the sandbox workspace root, normally `/workspace`
- destination: the caller's local repository/workspace root
- mirror cleanroom additions, modifications, and deletes from the included
  workspace file set into the local workspace
- if the source `/workspace` is a Git worktree, produce an export changeset
  against the recorded cleanroom baseline
- if the source `/workspace` is not a Git worktree, use raw workspace transfer
- honor Git ignore rules by default for Git-backed source workspaces; ignored
  paths such as `node_modules/`, `.venv/`, and cache directories are not
  export candidates
- refuse to overwrite or delete a local path that changed since the last
  copy/import baseline
- leave a recoverable export payload in Cleanroom state if local conflict
  checks fail

`workspace export --dry-run` answers "what would be written to my local
checkout?" This is intentionally different from `workspace diff`.

If local files diverged while the cleanroom was running, export should fail
closed and name the conflicting paths. A later force or overwrite mode can be
added if we have a concrete need, but the initial export path should be
conflict-safe.

### `cleanroom workspace diff`

Show how `/workspace` differs from the cleanroom's recorded workspace
baseline.

```sh
cleanroom workspace diff <sandbox-id>
cleanroom workspace diff --stat <sandbox-id>
cleanroom workspace diff --name-only <sandbox-id>
```

This command answers "what changed inside the cleanroom workspace?"

It does not answer "what would be written locally?" Use
`workspace export --dry-run` for that.

The baseline should be the cleanroom's own workspace baseline: the workspace
state created from the exact checkout or restored stage before workload changes
are considered user output. If a later `workspace copy` copied local edits into
the cleanroom, `workspace diff` may show those edits too. That is acceptable:
the command is scoped to cleanroom workspace state, not local export effects.

## Top-Level Automation

Top-level automation should compose the same workspace operations exposed by
the manual commands. `--copy` must behave exactly like
`cleanroom workspace copy` for the same source and destination; the only
difference is that the top-level command creates or selects the sandbox before
running the copy operation.

### `--copy`

Supported on:

- `cleanroom exec`
- `cleanroom console`
- `cleanroom create`

`--copy` replaces `--include-local-changes` as the top-level copy-in flag.

For Git-backed source workspaces, the implementation should reuse the existing
repository changeset path:

1. create or select the sandbox,
2. create a repository changeset from local additions, modifications, and
   deletes,
3. apply that changeset after the exact checkout is prepared in `/workspace`,
4. record local binding metadata so later `workspace copy`,
   `workspace export`, and automated exports know the local root.

For non-Git source workspaces, the same `--copy` flag should use raw workspace
transfer instead of a changeset.

The existing changeset path already has the right ignore shape: it stages the
working tree through Git, so ignored files are not part of the copied payload
unless they are tracked.

### `--export-on-exit`

Supported on:

- `cleanroom exec`
- `cleanroom console`

For an automatically-created sandbox, `--export-on-exit` should run
`workspace export` after the execution/session exits and before the CLI
terminates the sandbox.

For `--keep`, export should still run after the execution/session exits, but
the sandbox should remain available. The user can run more explicit
`workspace copy`, `workspace diff`, or `workspace export` commands later.

For `cleanroom create`, export-on-termination needs extra local binding
semantics. That should be a later phase. The first version should support
manual `workspace export <sandbox-id>` for sandboxes created with `create`.

### `--sync`

Supported on:

- `cleanroom exec`
- `cleanroom console`

`--sync` is a convenience alias for:

```sh
--copy --export-on-exit
```

Help text should state that equivalence literally. `--sync` should not be the
primitive that implementation code special-cases; it should resolve into the
directional copy/export options early in CLI parsing.

### Defaults

Workspace copy/export must be explicit. Even a source-scoped workspace mirror
can touch a large number of files, so making it implicit would add a large file
operation to ordinary executions and make the common command path unexpectedly
slow.

Defaults:

- `cleanroom exec`: exact committed checkout, no workspace copy/export unless
  `--copy`, `--export-on-exit`, or `--sync` is requested
- `cleanroom console`: exact committed checkout, no workspace copy/export
  unless requested
- `cleanroom create`: exact committed checkout unless `--copy` is requested
- CI or non-interactive execution: same exact committed checkout behavior
- reused sandboxes via `--in <id>`: no implicit copy/export unless requested
- snapshot-backed sandboxes via `--from <snapshot-id>`: no implicit
  copy/export in the first version
- export is never implicit except through explicit `--export-on-exit` or
  `--sync`

This deliberately avoids local-interactive detection, config defaults, and
environment overrides.

## Safety Model

### Direction is explicit

There is no bidirectional merge command in v1.

- `copy` means local to cleanroom
- `export` means cleanroom to local
- `diff` means cleanroom workspace against cleanroom baseline
- `sync` is a documented alias for copy plus export-on-exit

### Mirror semantics are scoped by the workspace file set

Workspace copy/export should mirror additions, modifications, and deletes in
the named direction, but only inside the included workspace file set. This
keeps the mental model simple without copying platform-specific dependency
trees:

- copy makes the included `/workspace` source set match the included local
  source set
- export makes the included local source set match the included `/workspace`
  source set

For Git-backed source workspaces, the default included file set is:

- tracked paths
- tracked paths deleted locally or inside the cleanroom
- untracked paths that are not ignored by Git's standard ignore rules
- paths previously copied by Cleanroom and recorded in the local binding
  manifest

Ignored paths are excluded by default for Git-backed sources in both
directions. In practice, that means `node_modules/`, `.venv/`, build caches,
and other ignored runtime outputs are not copied from macOS into Linux and are
not exported from Linux back to macOS.

The implementation should ask Git for this file set, using commands equivalent
to `git ls-files` with standard excludes or the existing temporary-index
changeset flow. It should not hand-parse `.gitignore`.

For non-Git source workspaces, the default included file set is the raw
workspace tree, excluding Cleanroom control metadata and destination-specific
unsafe paths. There is no Git ignore model to infer, so raw copy is literal.

The safety mechanism is conflict detection and dry-run output, not making
delete behavior a separate mode.

Delete propagation is also limited to the included file set. Git-backed export
should not delete ignored local paths, even if the cleanroom workspace does not
contain them.

If a user deliberately wants ignored artifacts, they should use `cleanroom cp`
for explicit paths or a future explicit include path. The MVP should not add a
global `--include-ignored` switch because it is too easy to accidentally
export platform-specific dependency trees.

### Export detects local divergence

Before exporting, the CLI should compare the current local paths that would be
written or deleted against the manifest recorded at the last `workspace copy`
or initial workspace import.

If a local target changed independently, export should refuse by default.

The refusal message should name the conflicting paths and suggest:

```sh
cleanroom workspace export --dry-run <sandbox-id>
```

The exported payload should remain available under Cleanroom state so users can
recover it manually if needed.

### No hidden mounts

Workspace copy/export should be implemented as explicit file transfer events,
not as a bind mount or backend-specific shared directory. That keeps the API
and security model backend-neutral.

## Transfer Representation

The implementation should not depend on the `rsync` binary being installed in
the guest. "Mirror" describes user semantics, not the wire protocol.

The lower transfer layer already owns the per-file and archive mechanics:

- stat one sandbox path
- walk one sandbox tree
- read one sandbox file as a bounded byte stream
- write one local file stream to one sandbox file
- remove one sandbox path
- archive sandbox paths to a bounded byte stream
- extract one archive stream into a sandbox directory

Workspace copy/export should add a higher-level planning representation that
is independent of transport:

- normalized relative path
- file type for regular files and directories
- mode bits needed for executable files
- content digest
- optional content payload
- delete marker
- included/excluded reason, so dry runs can explain when ignored paths are not
  candidates

The planner should then select one of two transports:

- Git changeset transport when the source workspace is a Git worktree and the
  destination has the compatible recorded baseline.
- Raw transfer transport when the source workspace is not a Git worktree.

If the source is Git but the destination cannot accept the changeset because
the baseline is missing or incompatible, fail with a clear error. Do not
silently fall back to raw copy, because that changes ignore/delete semantics.

For efficient raw-transfer implementation, the CLI can batch file changes into
the existing archive-write path plus a workspace manifest. Export can use the
inverse shape: build a manifest and payload from `/workspace`, stream it to the
CLI through archive/read primitives, and let the CLI apply it to the local
workspace after local conflict checks pass.

`cleanroom cp` remains the one-file convenience layer over the same substrate.
It should not grow workspace baselines or mirror behavior.

Git remains useful for baseline, ignore, and diff calculation when the source
workspace is a Git checkout. V1 should lean on that for safety while still
supporting non-Git workspaces through raw transfer.

## State and Metadata

Cleanroom should record workspace metadata per sandbox:

- sandbox ID
- workspace root inside the sandbox, normally `/workspace`
- local root path used for the last copy, stored client-side only
- cleanroom workspace baseline manifest or tree digest
- last copy manifest used for export conflict detection
- ignore/source file-set mode used for the last copy
- selected source transport, either Git changeset or raw transfer
- last copy time

The server should store sandbox/workspace facts needed to operate inside the
sandbox. The CLI should store local filesystem paths in local state, not in
portable server metadata, because the server may be remote and local paths may
be sensitive or meaningless elsewhere.

## API Shape

The public CLI should lead the design. The transfer substrate now provides the
low-level file/path/archive operations; workspace APIs should stay narrow and
workspace-aware:

- apply a workspace copy payload to a sandbox workspace, whether represented
  as a Git changeset or a raw transfer manifest
- produce a workspace diff against the cleanroom baseline
- produce a workspace export payload and manifest

Those operations should reject:

- missing sandbox ID
- stopped or non-ready sandbox
- sandbox with an active execution
- paths outside the workspace root
- path traversal entries

The workspace API should not reimplement `cleanroom cp`. It should consume the
existing transfer and repository-changeset substrates, then add workspace-root
scoping, baseline tracking, mirror planning, and conflict metadata.

## Interaction With Existing Local Changesets

`--include-local-changes` currently packages local dirty state as an explicit
repository changeset during sandbox creation.

This plan should rename that user-facing behavior to `--copy`:

- `--copy` is the top-level one-shot copy-in flag for local workspace changes
- `workspace copy` is the manual form of the same copy-in operation
- Git source workspaces use repository changesets
- non-Git source workspaces use raw workspace transfer
- `workspace export` is the explicit copy-out primitive
- `--export-on-exit` is the top-level copy-out automation
- `--sync` composes copy-in with export-on-exit

The term "repository changeset" should remain an implementation and diagnostic
term. CLI help and docs for the happy path should describe the behavior as
copying workspace changes.

Because Cleanroom is pre-1.0, prefer removing `--include-local-changes` rather
than keeping a long compatibility path. If a short alias is kept during
transition, it should be documented as equivalent to `--copy` and then deleted
before v1.

## CI and Hermetic Workflows

CI should remain straightforward:

```sh
cleanroom exec -- npm test
```

The default should continue to be exact committed checkout behavior:

- no workspace copy unless requested
- no export back to the checkout
- dirty worktree warning if relevant
- repository content comes from the resolved commit and explicit policy

No environment or config default should implicitly enable workspace copy/export.
CI, automation, and normal local executions get exact checkout behavior by
omitting workspace copy/export flags.

## Delivery Strategy

### Phase 0: Transfer substrate

Status: landed.

- `cleanroom copy` / `cleanroom cp` supports one local file to one sandbox path
  and one sandbox file to one local path.
- Backend-neutral transfer helpers cover stat, walk, bounded read, streaming
  write, remove, archive read, and archive write.
- `docs/plans/sandbox-transfer-invariants.md` owns the low-level invariants and
  adversarial file-transfer test matrix.

### Phase 1: Unified workspace copy-in

- Add a shared workspace copy planner used by both manual commands and
  top-level automation.
- Add `cleanroom workspace copy`.
- Add `cleanroom workspace copy --dry-run`.
- Add `--copy` to `exec`, `console`, and `create`.
- Use repository changesets when the source workspace is a Git worktree.
- Use raw workspace transfer when the source workspace is not a Git worktree.
- Add tests that ignored files and directories, including a representative
  `node_modules/`, are not included in Git-backed copy unless tracked.
- Add tests that `workspace copy` and top-level `--copy` produce the same
  destination contents for Git and non-Git sources.
- Remove or immediately deprecate `--include-local-changes` as the old spelling
  of the same behavior.
- Apply copied changes after exact checkout setup and before command execution
  or console entry.
- Store enough local binding metadata for later export work.
- Update help text and docs so `--copy` is the only recommended copy-in flag.

### Phase 2: Workspace export and diff

- Add `cleanroom workspace export --dry-run`.
- Add `cleanroom workspace export`.
- Add `cleanroom workspace diff`.
- Add export tests proving ignored cleanroom outputs are not written back to
  the local checkout by default.
- Require explicit sandbox IDs or support `--last` consistently with existing
  inspect commands.
- Store local binding metadata in client-side Cleanroom state.

### Phase 3: Safe top-level export automation

- Add `--export-on-exit` to `exec` and `console`.
- Add `--sync` as an alias for `--copy --export-on-exit`.
- Add integration tests for local divergence, delete propagation, and dry-run
  output.

### Phase 4: Optional live local-to-cleanroom copy

- Decide whether `--copy-live` is still needed after one-shot copy/export has
  been exercised.
- If implemented, add `--copy-live` to `exec` and `console`.
- Keep `--sync` as `--copy --export-on-exit` unless we explicitly choose a
  breaking semantic change.
- Add file watching for `exec --copy-live` and `console --copy-live`.
- Batch rapid local changes.
- Propagate local delete events without periodically deleting unrelated
  cleanroom-only generated files.
- Surface live copy failures without hiding workload output.
- Ensure live copy cannot race with an export operation.

### Phase 5: Export-on-terminate for kept sandboxes

- Decide whether `create` should record an export-on-termination binding.
- If implemented, make `cleanroom sandbox rm` run export before termination
  when a local binding exists.
- If export fails, refuse termination by default and provide an explicit
  discard flag.

## Open Decisions

- Whether custom `repository.path` values should be supported by workspace
  commands or whether workspace copy should standardize on `/workspace`.
- Whether export conflict override should be named `--force`, `--overwrite`,
  or left out until there is a concrete need.
- Whether workspace commands should support an explicit local root override, or
  only use the bound/calling repository root.
- What explicit-path UX should exist later for exporting ignored generated
  artifacts when `cleanroom cp` is too low-level.
- Whether live copy deserves a separate `--copy-live` flag after the one-shot
  workflow lands.
