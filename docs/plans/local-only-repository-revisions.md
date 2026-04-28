# Local-Only Repository Revisions Plan

## Summary

Teach `--include-local-changes` to include the full local repository state needed
to reproduce the current workspace:

- local-only committed Git history, carried as a Git bundle
- uncommitted and untracked worktree changes, carried as the existing changeset
  overlay

The later UX cleanup can rename or replace this behavior with `--copy-in`. For
this phase, keep the existing flag and broaden what it includes.

## Problem

Repo-aware commands currently resolve the local repository remote URL and
committed `HEAD`, then ask the control plane to materialize that exact checkout
inside the sandbox.

That fails when `HEAD` is a local-only commit because the remote-backed
repository store cannot fetch it. The commit object exists in the local clone,
but it has not been pushed to the configured remote.

The existing `--include-local-changes` path can package dirty worktree changes,
but it assumes the base commit is already fetchable by the sandbox. That means it
cannot represent the common workflow where a developer has both local commits and
uncommitted edits.

## Target Model

Without `--include-local-changes`, repo-aware checkout stays remote-backed. If
the resolved `HEAD` is not available from the configured remote, fail clearly and
suggest pushing the branch or using `--include-local-changes`.

With `--include-local-changes`, Cleanroom packages local repository state in two
layers:

1. A Git bundle for local-only committed history.
2. A worktree overlay for uncommitted and untracked changes.

The guest ends with:

- `HEAD` at the local commit SHA
- a dirty worktree only when the host worktree was dirty
- no host workspace mount
- no mutable commit state in `cleanroom.yaml`

## Git Bundle Layer

The CLI should create a bundle for objects reachable from local `HEAD` but not
reachable from the configured remote-tracking refs:

```sh
git bundle create "$bundle" HEAD --not --remotes="$remote"
```

If Git refuses to create an empty bundle, the local commit is already reachable
from the local remote-tracking refs and no bundle is needed. If the local
remote-tracking refs are stale or contain commits no longer available from the
actual remote, the control plane validation must catch that.

The control plane must verify:

- all bundle prerequisites are fetchable from the canonical remote
- the bundle is well-formed
- the bundle contains the requested target commit
- after remote checkout plus bundle fetch, the target commit is reachable

If the bundle has missing remote prerequisites, fail clearly instead of uploading
or accepting a large full-history bundle in the first version.

## API Shape

Start simple by sending the bundle inline on sandbox creation:

```proto
message CreateSandboxRequest {
  RepositoryCheckout repository_checkout = 5;
  RepositoryChangeset repository_changeset = 7;
  RepositoryCommitBundle repository_commit_bundle = 8;
}

message RepositoryCommitBundle {
  string format = 1;             // "git-bundle-v1"
  string target_commit_sha = 2;  // must match repository_checkout.commit_sha
  string digest = 3;             // sha256 of bundle bytes
  bytes bundle = 4;
}
```

Cleanroom should enforce an explicit bundle size limit before sending and after
receiving the request. A first default of 64 MiB is enough for typical short WIP
branches while still producing a clear failure mode for large histories.

## Guest Checkout Shape

For remote-available commits without `--include-local-changes`, checkout stays
unchanged.

For `--include-local-changes` with a local-only commit, bootstrap should fetch
the bundle before checkout verification:

```sh
git clone --filter=blob:none --no-checkout --progress "$remote" "$dest"
git -C "$dest" fetch --progress "$bundle_file" \
  "HEAD:refs/remotes/cleanroom/$source_id"
git -C "$dest" checkout -B "$branch" "$commit"
git -C "$dest" reset --hard "$commit"
git -C "$dest" clean -ffdx
```

The bundle can be delivered as a request payload and written to a temporary
bootstrap file before `git fetch`. It does not require the control plane to share
the caller's local filesystem, so the same transport can work for local and
remote control planes.

## Worktree Overlay Layer

After the guest checks out the local target commit, apply the existing
`--include-local-changes` worktree changeset if the host worktree has
uncommitted or untracked changes.

The overlay base should be the local target commit, not the remote merge base.
That preserves the current patch-style validation while allowing the target
commit to be supplied by the bundle layer.

## Cache and Identity

Cache lineage should use stable content identities:

- canonical remote URL
- target commit SHA
- bundle digest as transport integrity metadata
- existing worktree changeset ID when an overlay is present

Do not key cache reuse on request-scoped names such as `source_id`.

## Incremental Slices

1. Detect when the resolved repository commit is not fetchable from the
   configured remote.
2. Under `--include-local-changes`, create a Git bundle for objects reachable
   from `HEAD` and not reachable from the configured remote-tracking refs.
3. Add an inline `RepositoryCommitBundle` request payload for local-only
   committed history, with digest and size validation.
4. Verify bundle prerequisites against the canonical remote.
5. Update repository bootstrap to fetch the bundle before checking out the
   requested commit.
6. Keep the existing worktree overlay path, but base it on the bundled target
   commit.
7. Fail clearly when bundle prerequisites are missing or the bundle exceeds the
   configured size limit.
8. Add coverage for remote-available commits, local-only commits,
   local-only-plus-uncommitted changes, missing prerequisite failures, oversized
   bundles, and corrupted bundle payloads.

## Deferred

- Rename or replace `--include-local-changes` with `--copy-in` once the broader
  copy-in UX is ready.
- Client-streamed bundle upload for larger histories if the inline request limit
  proves too restrictive.
- Full-history bundles when no remote-available base exists.
- Local-only submodule commits. The first slice should fail with a targeted error
  if a submodule points at a commit the sandbox cannot fetch.
