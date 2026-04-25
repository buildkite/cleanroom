# Repository Bootstrap Acceleration Plan

**Spec reference:** `spec.md` sections 5.1.1, 6.2.3
**Status:** Proposed
**Last reviewed:** 2026-04-18

## Summary

Speed up repo-aware sandbox creation on cold hosts without introducing a
virtual filesystem layer.

This plan keeps the existing repository-bootstrap contract:

- Cleanroom resolves an exact remote URL and full commit SHA
- the sandbox receives an exact committed checkout
- warm hits continue to come from services/dependency/workspace stage caches

The change is in how Cleanroom prepares host-side git state for that exact
checkout. Today the cold path eagerly creates and refreshes a full bare mirror
per remote. That is simple and robust, but it over-fetches for the common
bootstrap case where we only need one exact commit and a small subset of
objects to get started.

The target model is:

- keep Cleanroom as the owner of exact-commit repository semantics
- keep `content-cache` as the owner of generic Git transport caching artifacts
- replace the host-side full mirror assumption with a more general
  repository-store abstraction
- first back that abstraction with today’s mirror behavior
- then add a Git-native partial/promisor backend
- optionally hydrate cold hosts from bundle artifacts before talking to origin
- later distribute services/dependency/workspace-stage caches across hosts to
  skip Git entirely on a hit

This plan intentionally leaves any git-aware VFS or lazy worktree mount out of
scope.

## Problem

The current cold-host path pays for more repository data than repo-aware
bootstrap needs:

1. the host ensures a local mirror contains the target commit
2. the sandbox then performs its own clone and checkout flow from the gateway

That design already helps with repeated sandbox creation on one host, but it is
not optimized for:

- first use on a brand-new host
- very large repositories where only a small slice is needed immediately
- cross-host fleets where many machines repeat the same initial bootstrap

The current mirror also leaks through the control plane as a local-path-based
primitive used for exact-commit file reads during dependency-stage keying. That
makes it harder to evolve the transport layer independently.

## Goals

- Reduce cold-host upstream bytes and latency for repo-aware bootstrap.
- Preserve exact-commit checkout semantics.
- Keep the implementation backend-agnostic.
- Keep host-side credentials and origin auth under Cleanroom control.
- Preserve the current warm path through services/dependency/workspace stage caches.
- Create a clean boundary between Cleanroom repository semantics and
  `content-cache` transport artifacts.

## Non-goals

- Building a virtual filesystem or placeholder worktree layer.
- Changing workspace-stage identity semantics unless checkout behavior changes.
- Making stage caches user-addressable through snapshot-style APIs.
- Solving every Git optimization in one slice.
- Replacing normal Git checkout inside the sandbox in the first phase.

## Design Principles

### 1. Exact-commit semantics stay in Cleanroom

Cleanroom owns:

- canonical remote identity
- exact commit resolution and verification
- repository bootstrap policy checks
- services/dependency/workspace-stage identity
- sandbox-visible checkout behavior

`content-cache` should not become a Git worktree engine or a repository
materializer.

### 2. Transport acceleration should be generic

`content-cache` is the right place for reusable transport artifacts that are
not specific to Cleanroom’s sandbox lifecycle, for example:

- cached Git HTTP responses and packfiles
- immutable Git bundle artifacts
- bundle manifests and retrieval heuristics

These accelerators should be consumable by Cleanroom, but not defined in terms
of workspace stages or sandbox IDs.

### 3. Repository access needs an abstraction above “bare mirror on disk”

The control plane needs an interface for exact-commit repository operations,
not direct knowledge of how the host stores Git state.

That interface should answer questions such as:

- can you ensure this exact commit is locally available?
- can you read this file at that commit?
- can you provide transport hints for downstream checkout?

The current mirror implementation can satisfy those questions, but the
interface should not require a full mirror forever.

### 4. Warm speed should still come from stage caches

This plan accelerates the cold path, but it does not replace the layered cache
model. The largest end-to-end wins still come from reusing services,
dependency, and workspace stages. Git acceleration reduces the cost of
producing those stages on cold hosts.

## Proposed Architecture

### 1. Introduce `RepositoryStore` in Cleanroom

Replace direct use of `GitMirrorStore` with a higher-level host-side repository
access abstraction.

Suggested initial shape:

```go
type RepositoryStore interface {
    EnsureCommit(ctx context.Context, remoteURL, commitSHA string, hints FetchHints) error
    ReadFileAtCommit(ctx context.Context, remoteURL, commitSHA, path string) ([]byte, error)
    TransportHints(ctx context.Context, remoteURL, commitSHA string, hints FetchHints) (RepositoryTransportHints, error)
}

type FetchHints struct {
    Branches []string
    Refs     []string
}

type RepositoryTransportHints struct {
    BundleListURL string
}
```

Notes:

- `EnsureCommit` is authoritative for exact-commit availability.
- `ReadFileAtCommit` replaces direct mirror-path reads in dependency-stage
  keying.
- `TransportHints` is optional and can stay empty in the first slice.
- The first implementation should be an adapter over today’s mirror-backed
  logic.

### 2. Add a partial/promisor backend in Cleanroom

After the interface exists, add a second backend that stores Git state using
partial clone / promisor mechanics rather than a full mirror.

Expected behavior:

- one host-local store per canonical remote
- initial fetches prefer `--filter=blob:none`
- the store records the remote as a promisor-capable upstream
- exact-commit verification remains mandatory
- fetch hints can prioritize specific refs or branches without changing stage
  identity
- if a remote does not support the needed features, Cleanroom falls back to the
  mirror backend

This backend should be invisible to callers apart from better cold-host
performance.

### 3. Keep guest bootstrap semantics stable at first

The first transport optimization slice should not change the sandbox-visible
checkout model.

That means:

- keep exact-commit checkout in the sandbox
- keep current repo-aware command UX
- keep current workspace-stage keying unless checkout behavior actually changes
- avoid sparse checkout by default in the initial rollout

This keeps correctness and cache identity stable while the host-side transport
layer evolves.

### 4. Add bundle hydration through `content-cache`

Once Cleanroom can consume repository transport hints, add generic Git bundle
support to `content-cache`.

This should cover:

- storing immutable Git bundle artifacts
- serving bundle lists / manifests
- supporting filtered bundle variants such as `blob:none`
- treating bundle retrieval as a generic transport optimization, not a
  workspace concept

Cleanroom can then use those artifacts opportunistically:

- if a bundle artifact is available, hydrate the repository store from it first
- then fetch only the delta needed to reach the exact target commit
- if no bundle is available, continue with direct origin access

This is the most promising Git-native accelerator for brand-new hosts in a
fleet because it removes repeated full-history bootstrap from origin.

### 5. Later distribute stage caches in Cleanroom

Distributed services/workspace/dependency stages are a separate, higher-level
feature.

They belong in Cleanroom because they are defined by:

- compiled policy
- runtime base
- exact repository checkout identity
- backend-specific snapshot materialization

These are not generic Git transport artifacts.

When cross-host stage distribution exists, a cold host can skip Git entirely on
cache hits. That is the end-state acceleration path for full sandbox startup,
not just clone cost.

## Repo vs `content-cache` Ownership

### Cleanroom

Belongs in this repository:

- `RepositoryStore` interface and implementations
- exact-commit repository availability checks
- dependency-stage key-file reads at exact commits
- repository bootstrap orchestration
- checkout-mode decisions such as sparse checkout, if introduced later
- services/dependency/workspace-stage identity and cache publication
- distributed services/dependency/workspace-stage caches

### `content-cache`

Belongs in `content-cache`:

- generic Git smart-HTTP response caching
- packfile/blob caching and indexing
- immutable bundle artifact storage and serving
- bundle manifests / lookup helpers
- generic OCI transport caching, unchanged

### Explicitly out of `content-cache`

Should not move into `content-cache`:

- workspace-stage semantics
- stage cache keys
- sandbox-specific repository bootstrap flow
- guest worktree/materialization behavior
- any VFS-like worktree projection layer

## Delivery Strategy

This should land as incremental slices with measurement between phases.

### Phase 1: RepositoryStore abstraction

Status: not started.

Work:

- introduce `RepositoryStore` in Cleanroom
- implement a mirror-backed adapter over today’s behavior
- migrate dependency-stage keying off direct mirror-path reads
- keep all external behavior unchanged

Success criteria:

- no change in bootstrap semantics
- no change in services/dependency/workspace-stage cache keys
- existing repository-bootstrap, dependency-stage, and services-stage tests
  still pass

### Phase 2: Partial/promisor repository store

Status: not started.

Work:

- add a host-local partial/promisor implementation
- fetch exact commits with `blob:none` by default
- accept branch/ref hints for transport only
- fall back to the mirror-backed store on unsupported remotes or feature gaps

Success criteria:

- cold-host upstream bytes decrease materially versus the full mirror path
- repo-aware bootstrap remains exact-commit correct
- dependency-stage key-file reads continue to work through the new abstraction

### Phase 3: Bootstrap integration without checkout changes

Status: not started.

Work:

- wire the new repository store into repository bootstrap orchestration
- keep sandbox checkout behavior unchanged
- surface any useful transport hints without changing stage identity

Success criteria:

- repo-aware `create`, `exec`, and `console` get faster on cold hosts
- no user-visible behavior regression
- warm services/dependency/workspace-stage hits remain unchanged

### Phase 4: Bundle hydration via `content-cache`

Status: not started.

Work:

- add generic Git bundle artifact support to `content-cache`
- let Cleanroom consume bundle hints opportunistically
- hydrate the repository store from bundles before origin fetch when possible

Success criteria:

- brand-new hosts bootstrap mostly from cached bundle artifacts when available
- origin traffic drops further on fleet-wide cold starts
- fallback behavior remains simple and robust

### Phase 5: Cross-host stage distribution

Status: not started.

Work:

- export/import services/dependency/workspace-stage caches across hosts

Success criteria:

- cold hosts with imported stage caches skip Git bootstrap entirely on hits
- end-to-end sandbox startup time moves closer to warm-host behavior

## Compatibility and Rollout

- The mirror-backed store should remain available as a conservative fallback.
- The initial rollout should prefer correctness and observability over maximum
  optimization.
- Partial/promisor mode should be gated behind explicit runtime configuration
  until proven on supported hosts and remotes.
- Bundle hydration should be opportunistic rather than mandatory.

## Verification

Every phase should be verified with three layers in parallel:

- contract tests for repository access behavior
- end-to-end repository-bootstrap correctness tests
- repeatable benchmark runs that distinguish cold-host and warm-cache cases

### Contract tests

Add a backend-agnostic `RepositoryStore` contract suite and run it against:

- the mirror-backed implementation
- the partial/promisor implementation
- any fallback wrapper that can switch between them

That suite should verify:

- `EnsureCommit` succeeds only when the exact requested commit is available
- `ReadFileAtCommit` returns the same bytes as `git show <sha>:<path>`
- fetch hints affect transport only, never returned content
- missing commit and missing file errors remain stable across implementations
- fallback from partial/promisor to mirror-backed behavior preserves exact
  results

### End-to-end correctness

Build on the existing repository-bootstrap, workspace-stage, and
dependency-stage tests with fixture repositories that assert:

- sandbox checkout `HEAD` exactly matches the requested full commit SHA
- checked-out file content matches a host-side manifest for that commit
- `git status --porcelain` is clean immediately after bootstrap
- warm workspace-stage hits still skip repository bootstrap when expected
- warm dependency-stage hits still skip both repository bootstrap and
  dependency bootstrap when expected
- warm services-stage hits still skip repository bootstrap plus dependency and
  services bootstrap when expected
- services/dependency/workspace-stage keys stay stable unless checkout
  semantics intentionally change

The same scenarios should be exercised against both repository-store backends
where practical.

### Benchmarks

Benchmarking should use:

- deterministic local fixture repositories in CI for regression detection
- representative real repositories in scheduled or manual runs

The benchmark surface should include at least:

- cold host, cold stage cache
- warm repository store, cold stage cache
- warm workspace stage

Measured outputs should include:

- total wall time for repo-aware sandbox creation
- repository-store ensure latency
- repository bootstrap execution latency
- bytes fetched from origin
- bytes satisfied from bundle or transport cache
- fallback rate from partial/promisor to mirror-backed behavior

The repository-bootstrap benchmark script should start its own isolated
`cleanroom serve` instance per scenario rather than trying to delete files
under a live daemon. That avoids false results from open metadata databases.

### Phase gates

Do not advance a phase unless it shows:

- no correctness regressions in the contract and end-to-end suites
- no unexpected services/dependency/workspace-stage cache-key churn
- a measurable cold-host improvement on at least one representative large repo
- no material warm-path regression relative to the current baseline

## Observability

Add metrics and debug logs for:

- repository store backend selected
- exact-commit ensure latency
- bytes fetched from origin
- bytes satisfied from bundle artifacts
- fallback rate from partial/promisor to mirror-backed behavior
- repo-aware bootstrap duration by phase

These measurements are necessary to decide whether later slices, especially
bundle hydration and cross-host stage distribution, are worth their complexity.

## Open Questions

- How aggressively should Cleanroom persist and refresh promisor state for one
  remote over time?
- Which remotes we care about in practice support the needed partial-clone
  behavior well enough for this to be the default?
- Should bundle artifacts be generated externally, by a separate publishing
  job, or eventually by host-side Cleanroom workflows?
- When sparse checkout is evaluated later for very large repos, should it be a
  transport hint only or a first-class repository bootstrap mode?

## Recommendation

The first worthwhile implementation slice is not a VFS and not cross-host stage
distribution.

It is:

1. introduce `RepositoryStore`
2. replace the full mirror assumption with a partial/promisor-capable backend
3. keep current checkout semantics stable

That is the highest-leverage path that fits the existing architecture and
improves cold-host performance without taking on a much larger worktree
virtualization project.
