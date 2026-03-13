# Repository Checkout Plan

## Summary

Make the top-level developer commands repo-aware and keep the `sandbox`
subcommands generic.

The intended UX split is:

- `cleanroom create`
- `cleanroom exec`
- `cleanroom console`

These commands read a `repository` block from `cleanroom.yaml`, inspect the
local git repository, and ask the server to materialize an exact checkout in the
sandbox before it becomes usable.

- `cleanroom sandbox create`
- `cleanroom sandbox ls`
- `cleanroom sandbox rm`

These commands remain the low-level surface. They do not infer anything from
the local repository unless explicit flags are provided.

Under the hood, the server should avoid hitting GitHub for every sandbox by
serving sandbox git traffic from a host-side mirror cache behind the existing
gateway.

That mirror cache should be implemented inside Cleanroom rather than by taking
on a dependency on an external proxy service. The useful precedent is
Buildkite's [`git-proxy-cache`](https://github.com/buildkite/git-proxy-cache):
copy the mirror and refresh approach, keep Cleanroom's own gateway identity and
credential model.

## Why this is the useful UX

Most users standing in a repo do not want to think about sandbox plumbing. They
want:

- the repo they are currently in
- checked out inside the sandbox
- at the exact commit they are currently on
- with commands starting in that checkout

That maps naturally onto the top-level repo-oriented commands.

At the same time, Cleanroom still needs a backend-neutral primitive for SDKs,
automation, and future workflows that are not tied to a local checkout. That is
what `cleanroom sandbox ...` should remain.

This means `cleanroom create` should stop being just an alias for
`cleanroom sandbox create`. They are different UX layers on purpose.

## Goals

- Make top-level commands work naturally from a git repository.
- Keep checked-in policy declarative rather than embedding moving SHAs.
- Preserve the current "no host workspace mount" rule.
- Materialize repositories inside the guest, not from a host bind mount.
- Keep credentials and upstream auth entirely host-side.
- Reduce upstream git traffic enough to avoid GitHub rate-limit pressure at
  scale.

## Non-goals

- Reintroducing host filesystem mounts.
- Putting mutable commit SHAs in `cleanroom.yaml`.
- Teaching backend adapters to implement git clone logic themselves.
- Making `darwin-vz` pretend to support persistent checked-out sandboxes before
  it actually can.
- Solving package-registry caching in the same slice.

## Config model

The checked-in config should describe repository behavior, not a concrete
resolved checkout.

Suggested shape:

```yaml
repository:
  mode: current-repo
  remote: origin
  path: /workspace
  submodules: false
```

Meaning:

- `mode: current-repo`
  The top-level repo-aware commands should inspect the local git repository in
  the current working tree.
- `remote: origin`
  Which remote to read the upstream URL from.
- `path: /workspace`
  Where the checkout should appear inside the guest.
- `submodules: false`
  Whether submodules should be materialized as part of startup.

This belongs in `cleanroom.yaml` because it is repo intent, not host-specific
runtime state.

## What should not go in config

Do not put the resolved commit in `cleanroom.yaml`.

For example, this is the wrong model:

```yaml
repository:
  url: https://github.com/org/repo.git
  commit: abc123...
```

Reasons:

- it turns a normal run into a config-editing problem
- it creates noisy churn in a checked-in file
- it does not match the common case of "run the repo I am currently in"

The resolved git checkout should be computed at runtime by the repo-aware CLI.

## Resolution model

The top-level commands should translate the checked-in repo intent into a
concrete checkout request.

For `repository.mode: current-repo`, the CLI should:

1. discover the local repository root
2. read the configured remote URL, for example `remote.origin.url`
3. resolve local `HEAD` to a full commit SHA
4. send the resolved remote URL and commit SHA to the server

This means the server API should receive a concrete checkout spec, not the
high-level `current-repo` concept.

That keeps server behavior deterministic and backend-neutral.

## Dirty working tree behavior

`current-repo` should mean committed git state, not an implicit host snapshot.

Recommended v1 behavior:

- allow the run, but warn clearly that only committed `HEAD` is materialized
- ignore uncommitted local changes
- keep any future "include dirty worktree" behavior as an explicit separate mode

That keeps the common path usable while still avoiding a misleading UX where
the sandbox silently includes an implicit host snapshot.

## Command behavior

### Repo-aware commands

`cleanroom create`, `cleanroom exec`, and `cleanroom console` should:

- load `cleanroom.yaml`
- inspect the local git repository when `repository.mode` requires it
- resolve a concrete checkout request
- create or reuse a sandbox whose filesystem already contains that checkout
- default the guest working directory to `repository.path`

That means:

- `cleanroom create` returns a sandbox with the repo already checked out
- `cleanroom exec -- go test ./...` starts in `/workspace`
- `cleanroom console` opens in `/workspace`

### Generic sandbox commands

`cleanroom sandbox create` should stay explicit and low-level.

Suggested flags:

```text
cleanroom sandbox create \
  --git-url https://github.com/org/repo.git \
  --git-commit <sha> \
  --git-ref refs/heads/main \
  --git-path /workspace
```

These flags are useful for automation, SDK parity, and cases where there is no
local repo to inspect.

## API model

The API should carry a resolved repository checkout, not the local CLI intent.

Suggested proto sketch:

```proto
message GitCheckout {
  string remote_url = 1;      // https://host/org/repo.git
  string commit_sha = 2;      // full 40-char SHA
  string ref = 3;             // optional hint for auditing/fetch policy
  string destination_dir = 4; // default /workspace
  bool submodules = 5;        // default false
}

message RepositoryCheckout {
  oneof source {
    GitCheckout git = 1;
  }
}

message SandboxOptions {
  int64 launch_seconds = 3;
  RepositoryCheckout repository = 4;
}
```

Two related additions are worth planning alongside this:

- a sandbox field that reports the default guest workdir
- an execution option for explicit guest `working_dir`

Do not overload the existing host-side CLI `--chdir` with guest path meaning.

## Lifecycle model

Repository checkout is part of sandbox creation, not a normal user execution.

Proposed flow:

1. Client loads policy and resolves repository settings.
2. Client sends `CreateSandbox` with a resolved repository checkout spec.
3. Control service validates:
   - backend exists
   - backend supports repository checkout
   - remote URL scheme is supported
   - remote host is allowed by the compiled policy on port `443`
4. Control service creates sandbox state in `PROVISIONING`.
5. Persistent backend provisions the sandbox.
6. Control service runs internal checkout commands inside that sandbox.
7. If checkout succeeds, sandbox transitions to `READY`.
8. If checkout fails, control service tears the sandbox down and returns an
   error.

The key contract is:

- `READY` means the repository is already present

## Ownership boundary

Repository checkout should be orchestrated by the control service, not embedded
inside backend adapters.

Why:

- the feature is a control-plane lifecycle concern
- both backends already share a "run a command in the guest" abstraction
- backend-specific git logic would duplicate behavior and drift
- caching, auth policy, and event semantics belong above the adapter layer

Backends should keep doing two things:

- provision sandbox state
- execute commands in the guest

The control service should decide when repository checkout runs and how its
state affects sandbox readiness.

## Backend support

### Firecracker

Support this first.

Firecracker already has persistent sandboxes, so "create once, checkout once,
run many commands" is a truthful contract.

### darwin-vz

Support the same repository checkout model as Firecracker.

The difference from Firecracker is not repository UX. It is gateway identity:

- Firecracker can identify sandboxes by guest source IP
- `darwin-vz` should identify sandboxes with a scope token carried to the host
  gateway because guests share NAT

The git path should still be the same:

- guest clone traffic goes to the Cleanroom host gateway
- auth stays on the host
- later in-guest `git fetch` and submodule operations continue to use the same
  gateway path

Suggested capability name:

```text
sandbox.repository_checkout
```

## Checkout strategy inside the guest

Start with a simple correctness-first flow:

1. create the destination parent
2. `git clone --filter=blob:none --no-checkout <remote_url> <destination_dir>`
3. `git -C <destination_dir> checkout --detach <commit_sha>`
4. verify `git -C <destination_dir> rev-parse HEAD`
5. optionally materialize submodules

This keeps the semantics obvious, prefers blobless transfer over sparse
checkout, and avoids backend-specific file seeding.

## Efficient upstream strategy

The simplest useful first step is not "every sandbox clones from GitHub" and
not "copy a bare repo into every VM".

The right first design is:

- sandboxes talk to the existing host git gateway
- the host gateway owns upstream auth
- the host gateway is backed by a local mirror cache

This is the smallest slice that gets:

- auth on the host instead of the guest
- cheaper repeated sandbox creation
- the same git semantics inside the guest

without also needing host-side workspace snapshotting.

### Initial slice

For the first performance-oriented slice:

- keep the guest bootstrap flow unchanged
- keep repository materialization inside the guest
- move auth fully into the host gateway
- add a host-side bare mirror cache per canonical upstream remote
- implement that mirror cache in-tree inside Cleanroom
- do not add pack-response caching in the first slice

That means sandbox bootstrap still does:

1. `git clone --filter=blob:none --no-checkout`
2. `git checkout --detach <commit>`
3. optional submodule materialization

but the clone traffic is served from a host-managed mirror whenever possible.

### Mirror cache model

Maintain a host-side bare mirror per canonical upstream remote, for example:

```text
$XDG_STATE_HOME/cleanroom/repos/<hash>.git
```

The mirror is owned by the host control plane and updated with host-side
credentials.

For the first slice, the gateway should serve that mirror via `git
http-backend` rather than trying to hand-roll more of git smart-HTTP.

The mirror store should be keyed by canonical remote URL and must be able to
block until a requested commit is present. A pure TTL-based "return stale
mirror and refresh in the background" policy is not enough for Cleanroom,
because repository bootstrap is pinned to an exact commit and must not race a
background fetch.

### Gateway behavior

When a sandbox clones `https://github.com/org/repo.git`, the existing git
rewrite still sends it to the host gateway.

The gateway then:

1. maps the request to the canonical upstream remote
2. checks policy as it does today
3. resolves host-side credentials for that remote
4. ensures a local mirror exists for that remote
5. fetches upstream when the requested repo state is missing or stale
6. coalesces concurrent refreshes with locking or singleflight
7. serves the request from the local mirror

That preserves:

- host-side auth
- policy enforcement
- normal git semantics inside the sandbox
- compatibility with later in-sandbox `git fetch` and submodule operations

And it sharply reduces:

- repeated GitHub API and git smart-HTTP traffic
- per-sandbox pressure on upstream rate limits

### Gateway auth model

The gateway should be the single source of truth for upstream git auth.

The initial provider chain should be:

1. explicit host env credentials
2. host `git credential fill`
3. future service-specific providers such as GitHub App installation tokens

This lookup should key off the canonical HTTPS remote URL, not just the host,
because host git credentials may be path-scoped.

The guest should not receive upstream credentials. Missing auth should fail
fast and non-interactively.

This is one of the reasons to keep the implementation in-tree. Cleanroom
already has the right sandbox identity model for both backends:

- source-IP identity for `firecracker`
- scope-token identity for `darwin-vz`

An imported proxy with its own inbound auth protocol would be the wrong
abstraction layer here.

## Why not depend on git-proxy-cache directly

`git-proxy-cache` is useful research and a good source of implementation ideas,
but it is not the right dependency boundary for Cleanroom.

Reasons:

- it is a standalone service, not a small reusable mirror library
- its inbound auth is Buildkite OIDC, not Cleanroom sandbox identity
- its upstream auth is GitHub App specific, not Cleanroom's provider chain
- its mirror freshness model is request-staleness oriented, not
  exact-commit-oriented
- its pack-response cache buffers full request and response bodies, which is
  more complexity and memory pressure than the first Cleanroom slice needs

Recommended reuse:

- copy the bare-mirror plus singleflight refresh approach
- keep Cleanroom's current gateway and credential resolution
- revisit pack-response caching only if profiling shows mirror-backed clones are
  still too slow

## Why not copy a bare repo into the VM

Do not make "copy host bare repo into guest" the primary path.

Problems:

- a bare repo is not what user commands operate on
- copying `.git` state per sandbox is still expensive
- it creates awkward backend-specific materialization paths
- it bypasses the clean "all git traffic goes through the gateway" model

If we want a further optimization later, add it as a separate materialization
mode, not as the default architecture.

Useful future optimizations:

- shallow mirror refresh when the requested ref allows it
- archive export mode for CI workloads that only need files, not `.git`
- mirror GC and retention policies
- optional pack-response caching if later profiling justifies it

## Next speed stage

If the mirror-backed gateway is still not fast enough, the next stage should be
commit-addressed workspace seeds.

That means:

- prepare a host-side checkout seed for `(remote, commit, submodules, mode)`
- clone or copy that seed into new sandboxes
- keep `origin` pointed at the gateway

That is the step that turns "avoid hammering GitHub" into "new sandboxes start
close to plain sandbox creation time".

It should be a follow-up stage, not part of the first gateway cache slice.

## Failure behavior

If repository checkout fails, sandbox creation should fail.

Recommended v1 behavior:

- return an error from `CreateSandbox`
- include useful checkout stderr in the failure where practical
- terminate the partially initialized sandbox best-effort

This keeps the lifecycle simple:

- the caller either gets a ready sandbox with source present
- or gets an error and no sandbox to reuse

## Event model

Record repository materialization as sandbox lifecycle events.

Suggested sequence on success:

1. `PROVISIONING`: sandbox provisioning started
2. `PROVISIONING`: repository checkout started
3. `READY`: sandbox created and repository checkout complete

Suggested sequence on failure:

1. `PROVISIONING`: sandbox provisioning started
2. `PROVISIONING`: repository checkout started
3. `FAILED`: repository checkout failed
4. `STOPPED`: cleanup completed

This is clearer than pretending the checkout is a normal user execution.

## Testing plan

### CLI

- `cleanroom create|exec|console` read `repository.mode: current-repo`
- local git resolution produces the expected remote URL and commit SHA
- dirty working tree handling is clear and deterministic
- `cleanroom sandbox create` explicit git flags serialize correctly

### Control service

- repository checkout blocks `READY` until complete
- checkout failure returns an error and tears the sandbox down
- unsupported backends reject the feature with a capability mismatch
- sandbox events reflect provisioning, checkout, and ready/failure states

### Gateway and caching

- cache miss populates a mirror and serves clone traffic
- cache hit avoids upstream fetches
- concurrent identical refreshes coalesce
- policy enforcement still applies before mirror access

### Integration

- end-to-end create sandbox with resolved checkout against a local test git
  server
- checked-out `HEAD` matches the requested commit
- later git operations in the sandbox continue to use the gateway path

## Recommended implementation order

1. Add the `repository` config block and repo-aware top-level CLI behavior.
2. In the first slice, let the CLI orchestrate checkout using the existing
   create-sandbox plus create-execution flow so the UX can land without API
   churn.
3. Break `create` away from being a plain alias of `sandbox create`.
4. Route repository git traffic for both backends through the host gateway.
5. Move git auth fully into the gateway using a provider chain that can use
   host git credentials.
6. Add the in-tree mirror-backed gateway cache to cut upstream load.
7. Add resolved repository checkout fields to the API.
8. Add `sandbox.repository_checkout` capability reporting.
9. Move checkout orchestration into the control service for persistent
   sandboxes.
10. Add generic explicit `sandbox create` git flags.
11. If needed, add commit-addressed workspace seeds as a second-stage startup
    optimization.

## Bottom line

The most useful model is:

- `cleanroom.yaml` expresses repository intent via a `repository` block
- top-level commands are repo-aware
- `sandbox` subcommands stay generic
- the CLI resolves the local repo into a concrete checkout request
- the control plane owns checkout orchestration
- the guest still clones through the gateway
- the gateway owns auth and is backed by a host mirror cache
- later, if needed, workspace seeds can make repeated sandbox creation much
  cheaper than recloning every time
