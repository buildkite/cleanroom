# Workspace Copy-In and Copy-Out Plan

**Spec reference:** `spec.md` sections 5.1.1, 5.4, 5.4.2
**Related plan:** `docs/plans/sandbox-transfer-invariants.md`
**Status:** In progress
**Last reviewed:** 2026-05-03

## Implementation Status

- Phase 1 copy-in support landed in PR #251.
- PR #272 landed the read-only Phase 2 foundation:
  Git-backed `workspace diff`, Git-backed `workspace copy-out --dry-run`,
  sandbox-root resolution from the recorded repository checkout, and local-root
  safety that requires a matching local Git checkout before copy-out planning.
- This changeset should land the first write-capable Phase 2 slice: Git-backed
  `workspace copy-out` applies sandbox changes to the matching local checkout,
  saves a recoverable patch and manifest before applying, refuses local baseline
  mismatches, and refuses target paths that changed independently.
- Local binding metadata, non-Git/raw copy-out writes, top-level `--copy-out`,
  and `--sync` remain follow-up work.

## Summary

Replace the current `--include-local-changes` UX with a clearer `--copy-in` flag
and add explicit workspace copy-in/copy-out primitives for the common "edit
locally, run in Cleanroom, bring changes back" workflow.

In this plan, "workspace" means the project tree inside the cleanroom at the
resolved repository path, normally `/workspace`. It does not mean the full
sandbox filesystem, and it does not create a host mount.

The user-facing workspace vocabulary is:

- `cleanroom workspace copy-in`
- `cleanroom workspace copy-out`
- `cleanroom workspace diff`

The direction is named by the verb:

- `workspace copy-in` mirrors the included local workspace file set into the
  cleanroom workspace.
- `workspace copy-out` mirrors the included cleanroom workspace file set back
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

Workspace copy-in/copy-out should reuse existing substrates rather than redefine
low-level copy semantics. For Git-backed source workspaces, copy-in and copy-out
should use repository changesets as the transport. For non-Git source
workspaces, they should use raw workspace transfer over the file/path/archive
layer. That is an implementation choice; the user-facing behavior should be
the same.

The higher-level automation should stay small and literal:

```sh
cleanroom exec --copy-in -- npm test
cleanroom exec --copy-out -- npm run fix
cleanroom exec --sync -- npm run generate
```

Top-level flags:

- `--copy-in` copies local workspace changes into the sandbox workspace before
  running. It is automation over the same operation as
  `cleanroom workspace copy-in`.
- `--copy-out` copies the included sandbox workspace file set back into
  the local workspace after the command or console session exits.
- `--sync` is equivalent to `--copy-in --copy-out`.

## Goals

- Make local development workflows feel direct: edits made locally appear in
  the sandbox workspace, and useful changes made there can come back.
- Keep CI and hermetic workflows explicit and available.
- Keep workspace copy-in/copy-out behavior as command primitives that can be used
  manually and by top-level automation.
- Support custom `repository.path` values rather than hardcoding workspace
  operations to `/workspace`.
- Keep ordinary `exec`, `console`, and `create` startup fast by avoiding
  implicit whole-tree copy operations.
- Replace `--include-local-changes` with `--copy-in` instead of carrying both
  names long term.
- Exclude ignored dependency/cache/runtime directories from copy-in/copy-out by
  default, so a Linux `node_modules/` never gets copied back to a macOS
  checkout unless the user deliberately asks for that exact path.
- Avoid host filesystem mounts and backend-specific file sharing assumptions.
- Make copy-out safe by default: no silent overwrite of unrelated local changes,
  a dry-run path for previewing local effects, and clear conflict reporting.
- Preserve the existing repo-aware checkout model for clean committed inputs.
- Reuse the sandbox file/path/archive transfer layer for payload movement.
- Keep `--copy-in` and `workspace copy-in` behavior identical; only the transport
  should differ between Git and non-Git sources.

## Non-goals

- General-purpose arbitrary path copy across the whole sandbox filesystem; that
  belongs to `cleanroom copy` / `cleanroom cp`.
- Bidirectional conflict resolution or a full file synchronization daemon.
- Making workspace copy-in/copy-out the default behavior for top-level commands.
- Making `cleanroom sandbox create` infer repository policy or local workspace
  state.
- Copying ignored workspace artifacts out by default.
- Adding a copy-out conflict override such as `--force` or `--overwrite` in the
  MVP.
- Supporting arbitrary local copy-out roots in the MVP.
- Running copy-out automatically from `cleanroom sandbox rm`.
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
detection, dry-run copy-out planning, and workspace mirror semantics are out of
scope for `cp`.

Workspace commands should use a shared workspace planner with
workspace-specific safety semantics. The planner selects the transport from
the source workspace:

- Git source: repository changeset.
- Non-Git source: raw workspace transfer using the backend-neutral
  file/path/archive layer.

Top-level `--copy-in` should call the same copy-in operation as
`cleanroom workspace copy-in`; it is not a separate changeset-only mode. The
first implementation keeps that guarantee for Git-backed workspaces. For
non-Git workspaces, `cleanroom workspace copy-in` can use raw transfer against an
existing sandbox, but top-level `--copy-in` must stay disabled until there is a
create-time raw workspace primitive that runs before dependency and service
bootstrap.

Workspace commands must operate on the sandbox's recorded workspace root. For
repository-backed sandboxes, that root is the resolved `repository.path`,
defaulting to `/workspace`. Commands should not silently assume `/workspace`
when the policy or request used a different repository path.

### `cleanroom workspace copy-in`

Copy the caller's local workspace into the cleanroom workspace.

```sh
cleanroom workspace copy-in <sandbox-id>
cleanroom workspace copy-in --dry-run <sandbox-id>
```

Default behavior:

- source: the caller's local repository/workspace root
- destination: the sandbox's recorded workspace root, resolved from
  `repository.path` at sandbox creation and normally `/workspace`
- mirror local additions, modifications, and deletes from the included
  workspace file set into the cleanroom workspace
- skip `.git/`
- if the source is a Git worktree, package and apply a repository changeset
- if the source is not a Git worktree, use raw workspace transfer, but refuse
  when the sandbox does not expose a recorded workspace root
- honor Git ignore rules by default for Git-backed source workspaces
- record the local binding and manifest needed for later copy-out conflict
  checks

`workspace copy-in --dry-run` reports the planned remote writes and deletes
without applying them.

Because the cleanroom side is disposable, copy-in can use mirror semantics
inside the included workspace file set. `workspace copy-in` and `--copy-in`
should share the same planner and produce the same cleanroom workspace contents
for the same source.

### `cleanroom workspace copy-out`

Copy cleanroom workspace changes back to the caller's local workspace.

```sh
cleanroom workspace copy-out <sandbox-id>
cleanroom workspace copy-out --dry-run <sandbox-id>
```

Default behavior:

- source: the sandbox workspace root, resolved from `repository.path` and
  normally `/workspace`
- destination: the caller's local repository/workspace root
- mirror cleanroom additions, modifications, and deletes from the included
  workspace file set into the local workspace
- if the source workspace is a Git worktree, produce a copy-out changeset
  against the recorded cleanroom baseline
- if the source workspace is not a Git worktree, use raw workspace transfer
- honor Git ignore rules by default for Git-backed source workspaces; ignored
  paths such as `node_modules/`, `.venv/`, and cache directories are not
  copy-out candidates
- refuse to overwrite or delete a local path that changed since the last
  copy-in or initial workspace baseline
- leave a recoverable copy-out payload in Cleanroom state if local conflict
  checks fail

`workspace copy-out --dry-run` answers "what would be written to my local
checkout?" This is intentionally different from `workspace diff`.

If local files diverged while the cleanroom was running, copy-out should fail
closed and name the conflicting paths. A later conflict override can be added
from concrete usage, but the initial copy-out path should be conflict-safe.

The first write-capable implementation is Git-backed only. Until local binding
metadata lands, unbound copy-out writes require the local checkout `HEAD` to
match the sandbox's recorded repository baseline. If it does not match, the
command saves the sandbox patch in Cleanroom state and refuses to write.

### `cleanroom workspace diff`

Show how the sandbox workspace differs from the cleanroom's recorded workspace
baseline.

```sh
cleanroom workspace diff <sandbox-id>
cleanroom workspace diff --stat <sandbox-id>
cleanroom workspace diff --name-only <sandbox-id>
```

This command answers "what changed inside the cleanroom workspace?"

It does not answer "what would be written locally?" Use
`workspace copy-out --dry-run` for that.

The baseline should be the cleanroom's own workspace baseline: the workspace
state created from the exact checkout or restored stage before workload changes
are considered user output. If a later `workspace copy-in` copied local edits into
the cleanroom, `workspace diff` may show those edits too. That is acceptable:
the command is scoped to cleanroom workspace state, not local copy-out effects.

## Top-Level Automation

Top-level automation should compose the same workspace operations exposed by
the manual commands. `--copy-in` must behave exactly like
`cleanroom workspace copy-in` for the same source and destination; the only
difference is that the top-level command creates or selects the sandbox before
running the copy-in operation.

### `--copy-in`

Supported on:

- `cleanroom exec`
- `cleanroom console`
- `cleanroom create`

`--copy-in` replaces `--include-local-changes` as the top-level copy-in flag.

For Git-backed source workspaces, the implementation should reuse the existing
repository changeset path:

1. create or select the sandbox,
2. create a repository changeset from local additions, modifications, and
   deletes,
3. apply that changeset after the exact checkout is prepared in the resolved
   sandbox workspace root,
4. record local binding metadata so later `workspace copy-in`,
   `workspace copy-out`, and automated copy-out operations know the local root.

For non-Git source workspaces, the same `--copy-in` flag should eventually use raw
workspace transfer instead of a changeset, but only once raw transfer can run as
part of sandbox creation before bootstrap.

The existing changeset path already has the right ignore shape: it stages the
working tree through Git, so ignored files are not part of the copied payload
unless they are tracked.

### `--copy-out`

Supported on:

- `cleanroom exec`
- `cleanroom console`

For an automatically-created sandbox, `--copy-out` should run
`workspace copy-out` after the execution/session exits and before the CLI
terminates the sandbox.

For `--keep`, copy-out should still run after the execution/session exits, but
the sandbox should remain available. The user can run more explicit
`workspace copy-in`, `workspace diff`, or `workspace copy-out` commands later.

For `cleanroom create`, the first version should support manual
`workspace copy-out <sandbox-id>` for sandboxes created with `create`.
`cleanroom sandbox rm` should not write back into the caller's checkout.

### `--sync`

Supported on:

- `cleanroom exec`
- `cleanroom console`

`--sync` is a convenience alias for:

```sh
--copy-in --copy-out
```

Help text should state that equivalence literally. `--sync` should not be the
primitive that implementation code special-cases; it should resolve into the
directional copy-in/copy-out options early in CLI parsing.

### Defaults

Workspace copy-in/copy-out must be explicit. Even a source-scoped workspace mirror
can touch a large number of files, so making it implicit would add a large file
operation to ordinary executions and make the common command path unexpectedly
slow.

Defaults:

- `cleanroom exec`: exact committed checkout, no workspace copy-in/copy-out unless
  `--copy-in`, `--copy-out`, or `--sync` is requested
- `cleanroom console`: exact committed checkout, no workspace copy-in/copy-out
  unless requested
- `cleanroom create`: exact committed checkout unless `--copy-in` is requested
- CI or non-interactive execution: same exact committed checkout behavior
- reused sandboxes via `--in <id>`: no implicit copy-in/copy-out unless requested
- snapshot-backed sandboxes via `--from <snapshot-id>`: no implicit
  copy-in/copy-out in the first version
- copy-out is never implicit except through explicit `--copy-out` or
  `--sync`

This deliberately avoids local-interactive detection, config defaults, and
environment overrides.

## Safety Model

### Direction is explicit

There is no bidirectional merge command in v1.

- `copy-in` means local to cleanroom
- `copy-out` means cleanroom to local
- `diff` means cleanroom workspace against cleanroom baseline
- `sync` is a documented alias for copy-in plus copy-out

### Workspace root follows repository.path

Workspace operations use the sandbox's recorded workspace root. For
repository-backed sandboxes, that is the resolved `repository.path`, defaulting
to `/workspace`.

This keeps workspace copy-in/copy-out aligned with existing command execution: if a
policy checks the repository out somewhere other than `/workspace`, copy-in,
copy-out, diff, and top-level automation operate on that same path.

### Mirror semantics are scoped by the workspace file set

Workspace copy-in/copy-out should mirror additions, modifications, and deletes in
the named direction, but only inside the included workspace file set. This
keeps the mental model simple without copying platform-specific dependency
trees:

- copy-in makes the included sandbox workspace source set match the included
  local source set
- copy-out makes the included local source set match the included sandbox
  workspace source set

For Git-backed source workspaces, the default included file set is:

- tracked paths
- tracked paths deleted locally or inside the cleanroom
- untracked paths that are not ignored by Git's standard ignore rules
- paths previously copied by Cleanroom and recorded in the local binding
  manifest

Ignored paths are excluded by default for Git-backed sources in both
directions. In practice, that means `node_modules/`, `.venv/`, build caches,
and other ignored runtime outputs are not copied from macOS into Linux and are
not copied out from Linux back to macOS.

The implementation should ask Git for this file set, using commands equivalent
to `git ls-files` with standard excludes or the existing temporary-index
changeset flow. It should not hand-parse `.gitignore`.

For non-Git source workspaces, the default included file set is the raw
workspace tree, excluding Cleanroom control metadata and destination-specific
unsafe paths. There is no Git ignore model to infer, so raw copy is literal.

The safety mechanism is conflict detection and dry-run output, not making
delete behavior a separate mode.

Delete propagation is also limited to the included file set. Git-backed copy-out
should not delete ignored local paths, even if the cleanroom workspace does not
contain them.

If a user deliberately wants ignored artifacts, they should use `cleanroom cp`
for explicit paths or a future explicit include path. The MVP should not add a
global `--include-ignored` switch because it is too easy to accidentally
copy out platform-specific dependency trees.

### Copy-out detects local divergence

Before copying out, the CLI should compare the current local paths that would be
written or deleted against the manifest recorded at the last `workspace copy-in`
or initial workspace baseline.

If a local target changed independently, copy-out should refuse by default.

The refusal message should name the conflicting paths and suggest:

```sh
cleanroom workspace copy-out --dry-run <sandbox-id>
```

The copied-out payload should remain available under Cleanroom state so users can
recover it manually if needed.

The MVP should not provide a conflict override such as `--force` or
`--overwrite`. A later override can be added from concrete usage.

### Local root binding is explicit

Workspace copy-out should write only to a trusted local root:

- Prefer the local root bound by the last `workspace copy-in` or top-level
  `--copy-in`.
- If there is no binding, allow copy-out from the current working tree only when
  its repository identity matches the sandbox repository identity.
- Refuse copy-out when the local root is ambiguous.

The MVP should not add an arbitrary `--local-root` override. That avoids
writing cleanroom copy-out results into the wrong checkout.

### No hidden mounts

Workspace copy-in/copy-out should be implemented as explicit file transfer events,
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

Workspace copy-in/copy-out should add a higher-level planning representation that
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
the existing archive-write path plus a workspace manifest. Copy-out can use the
inverse shape: build a manifest and payload from the sandbox workspace root,
stream it to the CLI through archive/read primitives, and let the CLI apply it
to the local workspace after local conflict checks pass.

`cleanroom cp` remains the one-file convenience layer over the same substrate.
It should not grow workspace baselines or mirror behavior.

Git remains useful for baseline, ignore, and diff calculation when the source
workspace is a Git checkout. V1 should lean on that for safety while still
supporting non-Git workspaces through raw transfer.

## State and Metadata

Cleanroom should record workspace metadata per sandbox:

- sandbox ID
- workspace root inside the sandbox, resolved from `repository.path` and
  normally `/workspace`
- local root path used for the last workspace copy-in or copy-out, stored
  client-side only
- cleanroom workspace baseline manifest or tree digest
- last workspace copy-in manifest used for copy-out conflict detection
- ignore/source file-set mode used for the last workspace operation
- selected source transport, either Git changeset or raw transfer
- last workspace operation time

The server should store sandbox/workspace facts needed to operate inside the
sandbox. The CLI should store local filesystem paths in local state, not in
portable server metadata, because the server may be remote and local paths may
be sensitive or meaningless elsewhere.

## API Shape

The public CLI should lead the design. The transfer substrate now provides the
low-level file/path/archive operations; workspace APIs should stay narrow and
workspace-aware:

- apply a workspace copy-in payload to a sandbox workspace, whether represented
  as a Git changeset or a raw transfer manifest
- produce a workspace diff against the cleanroom baseline
- produce a workspace copy-out payload and manifest

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

This plan should rename that user-facing behavior to `--copy-in`:

- `--copy-in` is the top-level one-shot copy-in flag for local workspace changes
- `workspace copy-in` is the manual form of the same copy-in operation
- Git source workspaces use repository changesets
- non-Git source workspaces use raw workspace transfer
- `workspace copy-out` is the explicit copy-out primitive
- `--copy-out` is the top-level copy-out automation
- `--sync` composes copy-in with copy-out

The term "repository changeset" should remain an implementation and diagnostic
term. CLI help and docs for the happy path should describe the behavior as
copying workspace changes.

Because Cleanroom is pre-1.0, prefer removing `--include-local-changes` rather
than keeping a long compatibility path. If a short alias is kept during
transition, it should be documented as equivalent to `--copy-in` and then deleted
before v1.

## CI and Hermetic Workflows

CI should remain straightforward:

```sh
cleanroom exec -- npm test
```

The default should continue to be exact committed checkout behavior:

- no workspace copy-in unless requested
- no copy-out back to the checkout
- dirty worktree warning if relevant
- repository content comes from the resolved commit and explicit policy

No environment or config default should implicitly enable workspace copy-in/copy-out.
CI, automation, and normal local executions get exact checkout behavior by
omitting workspace copy-in/copy-out flags.

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

- Add a shared workspace copy-in planner used by both manual commands and
  top-level automation.
- Add `cleanroom workspace copy-in`.
- Add `cleanroom workspace copy-in --dry-run`.
- Add `--copy-in` to `exec`, `console`, and `create`.
- Resolve the destination workspace root from `repository.path`, defaulting to
  `/workspace`.
- Use repository changesets when the source workspace is a Git worktree.
- Use raw workspace transfer for manual `workspace copy-in` when the source
  workspace is not a Git worktree.
- Reject top-level `--copy-in` for non-Git workspaces until raw transfer can be
  attached to sandbox creation before bootstrap.
- Add tests that ignored files and directories, including a representative
  `node_modules/`, are not included in Git-backed copy unless tracked.
- Add tests that `workspace copy-in` and top-level `--copy-in` produce the same
  destination contents for Git sources.
- Remove or immediately deprecate `--include-local-changes` as the old spelling
  of the same behavior.
- Apply copied changes after exact checkout setup and before command execution
  or console entry.
- Store enough local binding metadata for later copy-out work.
- Update help text and docs so `--copy-in` is the only recommended copy-in flag.

### Phase 2: Workspace copy-out and diff

- PR #272: add `cleanroom workspace copy-out --dry-run`.
- This changeset: add Git-backed `cleanroom workspace copy-out` writes.
- PR #272: add `cleanroom workspace diff`.
- PR #272: resolve the sandbox workspace root from `repository.path`, defaulting
  to `/workspace`.
- Covered by repository changeset tests: ignored cleanroom outputs are not
  included in Git-backed copy-out candidates by default.
- This changeset: add copy-out tests proving local divergence fails closed with
  no force or overwrite mode.
- This changeset: add copy-out tests proving an unbound copy-out refuses to
  write unless the current working tree matches the sandbox repository baseline.
- Require explicit sandbox IDs or support `--last` consistently with existing
  inspect commands.
- Store local binding metadata in client-side Cleanroom state.

### Phase 3: Safe top-level copy-out automation

- Add `--copy-out` to `exec` and `console`.
- Add `--sync` as an alias for `--copy-in --copy-out`.
- Add integration tests for local divergence, delete propagation, and dry-run
  output.

### Phase 4: Optional live local-to-cleanroom copy

- Defer `--copy-in-live` until one-shot copy-in/copy-out has been exercised.
- If implemented, add `--copy-in-live` to `exec` and `console`.
- Keep `--sync` as `--copy-in --copy-out` unless we explicitly choose a
  breaking semantic change.
- Add file watching for `exec --copy-in-live` and `console --copy-in-live`.
- Batch rapid local changes.
- Propagate local delete events without periodically deleting unrelated
  cleanroom-only generated files.
- Surface live copy failures without hiding workload output.
- Ensure live copy cannot race with a copy-out operation.

### Phase 5: Optional copy-out-on-terminate for kept sandboxes

- Do not make `cleanroom sandbox rm` write to the caller's checkout in the MVP.
- If copy-out-on-terminate is added later, require an explicit create-time opt-in
  such as `cleanroom create --copy-out-on-terminate`.
- If an explicit copy-out-on-terminate fails, refuse termination by default and
  provide an explicit discard flag.

## Resolved Decisions

- Workspace commands support custom `repository.path` values and default to
  `/workspace`.
- Copy-out has no conflict override in the MVP.
- Copy-out uses the bound local root or a matching current working tree; there is
  no arbitrary local-root override in the MVP.
- Ignored generated artifacts remain excluded by default. Future support should
  use explicit relative includes, for example
  `cleanroom workspace copy-out <sandbox-id> --include path/to/generated-file`.
- Live copy is deferred. `--sync` remains one-shot
  `--copy-in --copy-out`.
- `cleanroom sandbox rm` does not copy out automatically. Copy-out-on-terminate is
  future work and must require explicit opt-in.
