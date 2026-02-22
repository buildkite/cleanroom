# Git Gateway Design (MVP)

## Goal

Allow sandboxed workloads to use normal Git commands (`git clone`, `git fetch`)
while Cleanroom enforces explicit allowlist policy on remote Git destinations.

## Scope

This document describes the currently shipped MVP behavior.

Included:
- Git smart-HTTP read paths (`info/refs?service=git-upload-pack`, `git-upload-pack`)
- Policy-controlled allow/deny by host and optional repo
- Transparent Git URL rewrite inside the sandbox
- `upstream` and `host_mirror` source modes

Not included:
- Push/write flows (`git-receive-pack`)
- Auth token minting/injection pipeline
- Wildcard policy semantics

## Policy Model

Git controls live under `sandbox.git`:

```yaml
sandbox:
  git:
    enabled: true
    source: upstream # upstream | host_mirror
    allowed_hosts:
      - github.com
    allowed_repos:
      - org/repo
```

Behavior:
- `allowed_hosts` is required when `enabled: true`
- `allowed_repos` is optional; if omitted, any repo under allowed hosts is permitted
- Matching is exact and case-normalized in MVP

## Runtime Architecture

1. Shared host gateway
- Firecracker backend starts a shared HTTP Git gateway listener on host TCP.
- A single gateway process is reused across many sandboxes for parallel scale.

2. Per-sandbox scope registration
- Each sandbox/run registers an ephemeral scope token in an in-memory registry.
- Registry maps `scope -> compiled Git policy`.
- Scope is removed on run/sandbox cleanup.

3. Scoped rewrite in guest command environment
- Runtime injects per-host Git rewrite entries using `GIT_CONFIG_COUNT` style env.
- Rewrites:
  - from `https://github.com/...`
  - to `http://<sandbox-host-ip>:<gateway-port>/git/<scope>/github.com/...`

4. Gateway enforcement path
- Request route shape:
  - `GET /git/<scope>/<host>/<owner>/<repo>.git/info/refs?service=git-upload-pack`
  - `POST /git/<scope>/<host>/<owner>/<repo>.git/git-upload-pack`
- Gateway resolves policy by scope, then enforces:
  - host in `allowed_hosts`
  - optional repo in `allowed_repos`
- Deny responses use stable reason bodies (for example `host_not_allowed`,
  `repository_not_allowed`).

5. Source selection
- `upstream`: proxy request to upstream HTTPS remote
- `host_mirror`: serve from local bare mirror when available, otherwise fallback to upstream

## Security Properties

- Default deny for non-allowlisted destinations
- Per-sandbox policy isolation via scope token even with shared gateway
- No user-facing command changes required
- Header forwarding is allowlisted and explicit

## Validation Summary

Validated for MVP:
- Allowlisted clone succeeds
- Disallowed clone is rejected with explicit deny reason and `403`
- Parallel sandbox Git reads succeed through shared gateway

## Future Improvements

1. Request coalescing for `git-upload-pack`
- Coalesce identical in-flight upload-pack requests (likely keyed by scope, repo,
  and request body signature) to reduce upstream load under concurrent clones.

2. Response cache/spool layer for read paths
- Add bounded spool/cache for upload-pack responses, cache only successful
  responses, and stream to client while writing cache.

3. Stronger observability
- Emit structured allow/deny and upstream-source metrics (mirror hit/miss,
  upstream latency, in-flight coalescing stats).

4. Capability-driven transport adapters
- Keep shared tap TCP as baseline.
- Optionally add alternative transports (for example vsock where reliable) behind
  runtime capability checks.

5. Policy evolution
- Add deterministic wildcard/org-level matching with precedence rules.

6. Write support
- Add `git-receive-pack` with authenticated write policy and auditable decisioning.
