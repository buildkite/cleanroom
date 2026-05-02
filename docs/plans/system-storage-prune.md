# System Storage Prune Plan

**Related plan:** `docs/plans/layered-caching.md`
**Status:** In progress
**Last reviewed:** 2026-05-03

## Summary

Add a backend-agnostic storage inventory and prune workflow for host-side cleanroom state. The user-facing shape should feel like `docker system df` and `docker system prune`: first show where disk is going, then reclaim storage that cleanroom can prove is no longer referenced.

The immediate problem is orphaned host state. A local inspection showed `~/.local/state/cleanroom` at 471 GiB, with 360 GiB under `snapshots` and 109 GiB under `sandboxes`, while the snapshot metadata database reported zero tracked snapshots and `cleanroom sandbox ls --all` showed only one stopped sandbox. That means happy-path cleanup exists, but we need a way to discover and remove state that was left behind by crashes, old daemon versions, interrupted runs, or manual experimentation.

## Goals

- Provide `cleanroom system df` to summarize cleanroom-owned disk usage by category.
- Provide `cleanroom system prune` to remove reclaimable host storage safely.
- Preserve explicit user artifacts by default, especially named snapshots, images, repository mirrors, and content-cache blobs.
- Treat backend storage as an implementation detail; CLI flags and JSON output should describe cleanroom concepts rather than APFS, ext4, or VZ internals.
- Make dry-run output exact enough that users can see the paths, ids, byte counts, and reasons before deletion.
- Let future cleanup paths share one inventory model instead of adding independent ad hoc prune commands.

## Non-Goals

- Do not add background deletion in the first slice. The initial workflow is explicit and user initiated.
- Do not delete explicit snapshots by default. Users should opt in with `--all` or a snapshot-specific command.
- Do not blindly remove content-cache storage while the gateway may be using it.
- Do not make cache policy part of `cleanroom.yaml`. This is host maintenance, not project policy.
- Do not depend on sandbox id naming conventions such as old `cr_` or newer `cr-` prefixes.

## Command Model

Add a new `system` command group:

```console
cleanroom system df [--json]
cleanroom system prune [--dry-run] [--force] [--older-than duration] [--all]
```

`system df` prints a grouped inventory:

- Total bytes by category.
- Reclaimable bytes by category.
- Protected bytes by category.
- A short reason for protected entries when verbose or JSON output is requested.

`system prune` defaults to a conservative reclaim set:

- Orphan sandbox runtime directories not present in daemon state.
- Orphan backend snapshot directories not referenced by snapshot metadata or stage-cache metadata.
- Old execution artifacts that exceed the existing service retention window.
- Stale prepared runtime rootfs cache entries when they are not the current runtime key.

`system prune --all` expands the candidate set to system-managed caches:

- Stage-cache snapshots that are not currently referenced by a live sandbox.
- Pulled or imported image cache entries that are not currently used.
- Repository mirrors and content-cache storage only when the implementation can prune through the owning component safely, or when the daemon is stopped and the command can prove exclusive access.

Without `--force`, `system prune` should prompt on TTY and refuse on non-TTY. `--dry-run` never deletes and should be the recommended first command in docs and error hints.

## Storage Categories

| Category | Primary Path | Owner | Default Prune |
| --- | --- | --- | --- |
| Sandbox runtime dirs | `StateBaseDir()/sandboxes` | Backend adapters | Orphans only |
| Explicit snapshots | `StateBaseDir()/snapshots` plus `snapshotstore` | Snapshot service | No |
| Stage-cache snapshots | `StateBaseDir()/snapshots` plus `cachestore` | Dependency cache | Only with policy or `--all` |
| Images | `CacheBaseDir()/images` plus image metadata in state | Image manager | No |
| Runtime rootfs cache | `CacheBaseDir()/<backend>/runtime-rootfs` | Backend adapters | Stale entries |
| Repository mirrors | `StateBaseDir()/repos` | Git gateway | No |
| Content cache | `CacheBaseDir()/content-cache` | Content-cache gateway | No |
| Execution artifacts | `StateBaseDir()/executions` | Control service | Old retained artifacts |
| Changesets | `StateBaseDir()/changesets` | Changeset store | No |

The important distinction is between immutable byte reuse and guest-prepared state reuse. Content-cache and repository mirrors speed up transferring source bytes into cleanroom. Stage-cache snapshots and runtime rootfs caches are prepared guest state. Pruning one should not imply pruning the other.

## Inventory Model

Introduce a small internal package, for example `internal/storagegc`, that owns inventory and prune planning:

```go
type Entry struct {
    Kind        string
    ID          string
    Backend     string
    Path        string
    StorageRef  string
    SizeBytes   int64
    Reclaimable bool
    Reason      string
    ProtectedBy []string
    CreatedAt   time.Time
    LastUsedAt  time.Time
}

type Report struct {
    Entries []Entry
    Totals  map[string]CategoryTotal
}
```

The package should expose three phases:

- `Inventory`: read daemon state, metadata stores, runtime config, and known storage roots.
- `PlanPrune`: apply the selected retention policy and produce ordered delete actions.
- `ExecutePrune`: perform deletes through typed owners where possible and raw filesystem deletes only for proven orphans.

This keeps `internal/cli/system.go` small and makes the core behavior testable without invoking the CLI.

## Protection Rules

The inventory should build a reference set before considering deletion:

- Live daemon sandbox ids from the control service protect matching sandbox runtime directories.
- Explicit snapshot records from `snapshotstore.List` protect their `StorageRef`.
- Stage-cache records from `cachestore.List` protect their backing snapshot storage unless the prune policy explicitly includes stage caches.
- Known image records protect image filesystem layers and metadata.
- The currently selected backend runtime rootfs key protects the matching prepared runtime rootfs file.
- Recent execution artifacts remain protected according to the existing control-service retention settings.

Entries that are missing metadata but live under a cleanroom-owned root can become orphan candidates. Entries outside known cleanroom roots should never be deleted by this command.

## Deletion Rules

Use typed deletion paths when metadata exists:

- Stage-cache records should be deleted through `cachestore.Delete` and the owning volume store deletion path.
- Explicit snapshots should be deleted only by snapshot-specific commands or `system prune --all --snapshots`, through `snapshotstore.Delete` and the owning volume store.
- Images should be deleted through the image manager.

Use raw filesystem deletion only when all of these are true:

- The path is under a known cleanroom state or cache root.
- No metadata store references it.
- No live daemon state references it.
- The directory shape matches a storage category scanner owns.

This is what lets us clean up abandoned directories from older versions without teaching every historical format to the current metadata stores.

## Implementation Slices

1. Add `cleanroom system df --json` with inventory only.
   - Add `System SystemCommand` to the CLI root.
   - Scan known roots and metadata stores.
   - Report protected and reclaimable bytes without deleting anything.
   - Add tests using temporary XDG state, cache, and data directories.

2. Add conservative `cleanroom system prune`.
   - Support `--dry-run`, `--force`, and TTY confirmation.
   - Delete only orphan sandbox runtime dirs, orphan snapshot dirs, stale runtime rootfs files, and old execution artifacts.
   - Keep raw filesystem deletion limited to proven orphan paths.

3. Add policy-backed cache pruning.
   - Support `--older-than` for stage caches and runtime rootfs caches.
   - Touch stage-cache records on restore so age-based pruning reflects actual reuse.
   - Prefer count or byte-limit internals later, but keep the first public interface duration-based.

4. Add explicit `--all` expansion.
   - Include image cache pruning through the image manager.
   - Include repository and content-cache pruning only via safe owner APIs or offline-only checks.
   - Print a stronger confirmation prompt that names the expanded categories.

5. Add automatic cleanup hooks after the explicit command is proven.
   - On daemon startup, mark obvious orphan entries for later pruning but do not delete immediately.
   - After sandbox termination, opportunistically remove runtime dirs that belong to the terminated sandbox.
   - Consider a bounded background prune for system-managed caches only after the manual command has telemetry and tests.

## Tests

- Inventory reports byte totals for every known category.
- Referenced snapshot and stage-cache storage is protected.
- Unreferenced snapshot directories under the backend snapshot root are reclaimable.
- Active sandbox runtime directories are protected even if their filesystem metadata is stale.
- Orphan sandbox runtime directories are reclaimable without relying on id prefix format.
- `system prune --dry-run` returns the same planned actions without deleting paths.
- `system prune` refuses to delete in non-interactive mode without `--force`.
- Raw filesystem deletion refuses paths outside cleanroom-owned roots.
- JSON output is stable enough for scripts and future UI use.

## Open Questions

- Should the default `system prune` include stopped but still known sandboxes, or only unknown orphan sandbox directories? The safest first slice is unknown orphans only.
- Should explicit snapshot pruning live under `system prune --all --snapshots`, or should it remain only under `cleanroom snapshot rm`? Keeping it snapshot-specific is less surprising.
- Should content-cache get its own owner-level prune API first? That is preferable before exposing content-cache deletion through `system prune --all`.
- Should automatic cleanup run in the daemon or remain a CLI-initiated maintenance command? The explicit command should come first so the deletion model is observable and testable.

## Recommended First Slice

Build `cleanroom system df --json` and `cleanroom system prune --dry-run` first. That gives immediate visibility into disk use and validates the reference graph without risking deletion. Once the dry-run output correctly identifies the current orphan snapshot and sandbox storage, enable conservative deletion behind `--force`.
