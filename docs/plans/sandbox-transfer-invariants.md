# Sandbox Transfer Invariants

**Status:** Stabilization plan
**Last reviewed:** 2026-04-26

## Summary

Cleanroom now has explicit primitives for moving files and archive payloads into
and out of persistent sandboxes. These primitives are intended to be the
substrate for one-off `cleanroom copy` usage first, and later for faster
workspace sync, diff, and export workflows.

The transfer layer should stay backend-neutral above the final command-runner
boundary. Firecracker and Darwin-VZ can differ in how they execute a guest
command and attach streams, but file semantics should not diverge between
backends.

## Scope

The current transfer surface covers:

- stat one sandbox path
- walk one sandbox tree
- read one sandbox file as a bounded byte stream
- write one local file stream to one sandbox file
- remove one sandbox path
- archive sandbox paths to a bounded byte stream
- extract one archive stream into a sandbox directory
- CLI `cleanroom copy` / `cleanroom cp` for one local file to sandbox path or
  one sandbox file to local path

The current surface does not cover recursive local directory upload,
sandbox-to-sandbox copy, full workspace sync, conflict detection, rsync-style
delete behavior, or patch/diff generation. Those should build on this layer
after these invariants are stable.

## Required Invariants

### Shared backend behavior

- All sandbox paths accepted by transfer helpers are absolute.
- Missing sandbox paths are reported through a typed not-found error, so the
  control service can map them to `NotFound` without string matching in the CLI.
- Guest stderr is preserved for failed transfer commands and used as the
  primary error detail.
- Bounded reads and archives fail once emitted data would exceed `max_bytes`.
  Any allowed prefix may be emitted before the limit error is returned.
- Streaming writes do not wait for all stdin to be copied before reading the
  guest response. Early guest failures such as directory destinations must be
  able to return without deadlocking behind a blocked stdin writer.

### File upload behavior

- Uploading a file preserves the source mode bits sent by the caller.
- Uploading a file preserves the source mtime sent by the caller.
- A missing parent directory is created before writing the file.
- An existing directory destination is rejected.
- An existing symlink-to-directory destination is rejected.
- An existing file symlink destination is preserved. The payload is written to
  the symlink target, not over the symlink itself.
- File symlink chains are followed with a bounded hop count. Intermediate
  symlinks are preserved and the final target receives the payload.
- Symlink loops fail cleanly before any final rename.
- Writes are atomic relative to the final target path: data is written to a
  temporary file beside the target, metadata is applied, and then the temporary
  file is renamed into place.

### File download behavior

- Copying from a sandbox to a local file installs via a temporary file in the
  destination directory and then renames into place.
- A local directory destination appends the remote basename.
- A missing slash-suffixed local destination is rejected instead of creating an
  ambiguous directory.
- Existing local file symlink destinations are preserved. The payload is
  installed through the symlink target rather than replacing the symlink.
- Local destination symlink chains are followed with a bounded hop count.
- Dangling local destination symlinks materialize their target path.
- Local destination symlink loops fail cleanly.
- Copying a sandbox file to a local file preserves the sandbox file mode and
  mtime.

### CLI copy behavior

- Exactly one operand must be remote in the form `<sandbox-id>:/absolute/path`.
- Local-to-local and sandbox-to-sandbox copies are rejected.
- Local-to-sandbox copies reject local directories until recursive upload is a
  deliberate feature.
- Local-to-sandbox copies send the local file mode and mtime to the backend.
- A slash-suffixed sandbox destination appends the local basename.
- An existing sandbox directory destination appends the local basename.
- An existing sandbox symlink-to-directory destination, including a symlink
  chain, appends the local basename while preserving the original destination
  spelling.
- Sandbox destination symlink loops fail before opening the local source for
  upload.

## Backend Boundary

The useful refactor boundary is:

```go
type SandboxCommandRunner func(
    ctx context.Context,
    sandboxID string,
    cmd []string,
    stream backend.OutputStream,
) (*backend.ExecutionResult, error)
```

`backend.SandboxFileTransfer` owns the invariant-heavy methods above and calls
a backend-specific command runner. Firecracker and Darwin-VZ keep sandbox
lookup, boot timeout, VM lifecycle, vsock or guest-agent execution, and test
hooks inside their adapters.

This avoids duplicating transfer semantics while keeping the backend internals
explicit and local.

## Adversarial Test Matrix

Backend script and adapter tests should cover:

- mtime and mode preservation
- direct directory rejection
- symlink-to-directory rejection
- symlink target preservation
- symlink chain target preservation
- symlink loop failure
- early guest failure while stdin is still blocked
- typed not-found mapping from guest stderr
- bounded read/archive errors

CLI tests should cover:

- operand classification and unsupported direction errors
- local directory upload rejection
- mode and mtime forwarding
- remote directory and remote symlink-to-directory basename inference
- remote symlink cycle rejection
- local directory destination basename inference
- local symlink, symlink-chain, dangling-symlink, and symlink-cycle download
  destinations
- remote read and write error propagation

## Delivery Guidance

Do not broaden this PR into workspace sync. The stabilization target is a small,
well-defined transfer layer whose behavior is hard to regress. Once this matrix
is covered, workspace sync can layer higher-level planning, change detection,
and delete behavior on top without reopening backend copy semantics.
