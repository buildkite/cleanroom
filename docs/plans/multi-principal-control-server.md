# Multi-Principal Control Server Authorization Plan

**Spec reference:** `docs/spec.md` sections 5.4, 6.1, and 6.2; `docs/api.md`; `docs/tls.md`
**Status:** Slice 1 in progress
**Last reviewed:** 2026-05-25

## Summary

Allow many bots to share one Cleanroom server without being able to see, reuse,
or mutate each other's sandboxes, executions, snapshots, files, ports, or stage
caches.

The first durable shape should separate three concerns:

- OIDC JWT validation establishes who the caller is.
- Cleanroom server authorization decides what that caller can do.
- Repository policy still decides what a sandbox workload can reach once it is
  running.

Use OIDC JWTs as the first identity provider because they fit bot and CI
deployments without requiring a SPIFFE/SPIRE control plane. Keep the
authorization model Cleanroom-native: typed actions, typed resources,
server-stamped ownership, and optional CEL conditions over normalized request
data. Do not try to encode Cleanroom safety with broad OAuth scopes alone.

## Current Progress

Slice 1 now has the backend-neutral auth engine and local operator check path:

- runtime config accepts OIDC issuer trust roots and an auth policy file
- OIDC JWT validation covers issuer, audience, signature, expiry, `kid`,
  allowed algorithm, clock skew, and maximum token lifetime
- auth policies bind validated token claims to derived Cleanroom principals
- typed grants compile CEL conditions against a bounded request schema
- `cleanroom auth check` evaluates a token, local policy, action, resource, and
  JSON request fixture without starting the control server

Control server behavior is intentionally unchanged in Slice 1. Enforcement,
ownership stamping, embedded `content-cache` gateway envelopes, and stage-cache
partitioning remain Slice 2 work.

Focused validation run on 2026-05-25:

```text
mise exec -- go test ./internal/authz ./internal/runtimeconfig ./internal/cli
```

Result: passed.

Repository validation run on 2026-05-25:

```text
mise run check
```

Result: passed.

## Problem

Cleanroom's control server is currently designed around a trusted creator. That
is fine for local use and a single trusted automation, but it is not enough for a
shared server used by many bots.

Current state:

- HTTPS transport is server-auth TLS only. Client certificate authentication is
  explicitly unsupported today.
- `cleanroom serve` installs observability interceptors but has no control-plane
  caller identity for normal sandbox, execution, snapshot, or file APIs.
- Most ConnectRPC handlers call directly into `controlservice`.
- `Sandbox`, `Execution`, `Snapshot`, stage-cache, and snapshot metadata do not
  carry an owner or authorization scope.
- The client compiles policy and sends it to `CreateSandbox`, so a shared server
  must validate requested policy and repository inputs before it uses host-side
  credentials, mirrors, or backends.

Without a server-side authorization boundary, any caller that can reach the
control API can try global resource IDs directly, list retained state, attach to
executions, read sandbox files, restore snapshots, or warm-hit caches created by
another caller. Random resource IDs are not an access-control mechanism.

## Goals

- Authenticate remote bot callers with OIDC JWT bearer tokens.
- Validate trusted issuers, audiences, expiry, and signing keys server-side.
- Derive a stable Cleanroom principal from trusted token claims.
- Stamp every created resource with immutable server-derived ownership.
- Enforce exact-principal ownership first: by default, a principal can only
  access resources it created.
- Keep an authorization scope field for later controlled sharing, but do not
  make scope sharing the first default.
- Support typed grants for Cleanroom actions and resource kinds.
- Support CEL conditions inside typed grants for request constraints.
- Constrain `CreateSandbox` before repository mirrors, host credentials, stage
  caches, snapshots, or backend work can be used.
- Filter list APIs at the resource store or state snapshot boundary.
- Partition stage-cache lookup and publication by owner so warm caches do not
  become cross-principal side channels.
- Fail closed when identity, grant matching, CEL evaluation, owner lookup, or
  resource metadata is ambiguous.
- Keep auth configuration in runtime/server config, not in `cleanroom.yaml`.

## Non-goals

- Do not introduce SPIFFE or mTLS in the first implementation.
- Do not make Cleanroom an OAuth authorization server.
- Do not mint delegated child-bot tokens in the first implementation.
- Do not add a user or service-account management database.
- Do not support broad relationship-based sharing in the first implementation.
- Do not move sandbox egress policy into the auth policy.
- Do not expose host secrets or upstream credentials to guests.
- Do not use OPA, Cedar, OpenFGA, or Zanzibar as the first authorization engine.
- Do not add policy mutation or prompt-time approval flows.

## Target Model

### Identity

Remote callers send an OIDC JWT access token:

```text
Authorization: Bearer <jwt>
```

The server validates:

- issuer matches a configured trusted issuer
- audience contains a configured Cleanroom audience
- token is not expired and not before its validity window
- signing key matches issuer JWKS and allowed algorithms
- token lifetime and clock skew fit configured bounds
- a configured binding maps the validated claims to a Cleanroom principal

The derived principal is an internal value, not raw user input:

```go
type Principal struct {
    ID      string // stable Cleanroom principal ID
    Subject string // original OIDC subject for audit
    Issuer  string // configured issuer name or URL
    Scope   string // grouping label for future sharing and cache partitioning
}
```

The principal ID should be stable across server restarts and should include the
issuer identity. A safe default is `oidc:<issuer-name>:<sub>`, after canonical
normalization. Do not use the raw `sub` alone because different issuers can reuse
subject strings.

### Runtime Config

The main runtime config should define trust roots and point to authorization
bindings. It should not require listing every principal for dynamic bot fleets.

```yaml
auth:
  required: true
  oidc:
    issuers:
      - name: github-actions
        issuer: https://token.actions.githubusercontent.com
        audiences:
          - cleanroom
        jwks_url: https://token.actions.githubusercontent.com/.well-known/jwks
  policy_file: ~/.config/cleanroom/auth-policy.yaml
```

The policy file maps trusted claims to derived principals and grants:

```yaml
bindings:
  - name: cleanroom-repo-bots
    when: >
      token.issuer == "github-actions" &&
      claims.repository == "buildkite/cleanroom"
    principal:
      id: 'oidc:${token.issuer}:${claims.sub}'
      scope: 'repo:${claims.repository}'
    grants:
      - actions:
          - sandbox.create
          - sandbox.get
          - sandbox.list
          - sandbox.terminate
          - execution.create
          - execution.get
          - execution.list
          - execution.stream
        resources:
          - sandbox
          - execution
        condition: >
          request.repository.remote_url == "https://github.com/buildkite/cleanroom.git" &&
          request.backend in ["darwin-vz"] &&
          request.policy.resources.vcpus <= 4 &&
          request.policy.resources.memory_bytes <= 8589934592 &&
          request.policy.docker.required == false &&
          request.policy.network_default == "deny"
```

The exact schema can change during implementation, but the ownership boundary
must not: the server derives identity and grants from validated claims, not from
client-provided owner fields.

### Actions

Use explicit action names. This keeps CEL conditions from becoming the only
source of truth for what an expression is allowed to affect.

Initial action set:

```text
sandbox.create
sandbox.get
sandbox.list
sandbox.terminate
sandbox.file.stat
sandbox.file.walk
sandbox.file.read
sandbox.file.write
sandbox.file.remove
sandbox.file.archive
sandbox.file.extract
sandbox.port.dial

execution.create
execution.get
execution.list
execution.inspect
execution.cancel
execution.stdin.write
execution.stdin.close
execution.stream
execution.attach

snapshot.create
snapshot.get
snapshot.list
snapshot.delete
snapshot.restore
```

Cache-peer actions should stay separate from normal bot permissions because
cache replication already has a peer-token model:

```text
cache_peer.lookup
cache_peer.export
```

### CEL Conditions

Use CEL for conditional grants over a small typed input schema:

```text
principal.id
principal.issuer
principal.subject
principal.scope
claims
action
resource.kind
resource.id
resource.owner.principal_id
resource.owner.scope
request
```

Rules:

- compile and type-check all CEL expressions at server startup or config reload
- reject unknown fields at config load time
- deny on CEL runtime error
- set an evaluation cost limit
- do not expose secret values, bearer tokens, or host credential material to CEL
- expose normalized request data, not raw protobufs with unstable internals
- keep helper functions minimal and deterministic

CEL grants should only narrow typed grants. They must not be able to invent new
actions, skip ownership checks, or grant access to resources outside the server's
resource model.

## Resource Ownership

Every resource created through an authenticated request should be stamped by the
server:

```go
type ResourceOwner struct {
    PrincipalID string
    Scope       string
}
```

The first rule is exact ownership:

```text
caller.principal_id == resource.owner.principal_id
```

`Scope` is retained for future controlled sharing and cache partitioning, but
scope should not allow access by itself in the first implementation. This avoids
accidentally making a repo-level scope such as `repo:buildkite/cleanroom` a
shared resource pool before the sharing semantics are deliberate.

Ownership rules by resource:

- Sandboxes are owned by the principal that created them.
- Executions inherit ownership from their sandbox.
- Interactive sessions inherit ownership from their execution.
- Snapshots inherit ownership from the source sandbox.
- Sandboxes restored from snapshots require same-principal snapshot ownership in
  the first implementation.
- Stage-cache records are stamped with the creating principal and scope.
- Cache lookup only considers records owned by the caller unless a later sharing
  model explicitly allows scope-level reuse.

Client requests must not contain owner fields. If a future API returns owner
metadata, it should return display-safe fields and never trust echoed owner data
on writes.

## Authorization Flow

### Unary RPCs

1. Authenticate the request and place `Principal` in context.
2. Classify the handler into a typed `Action`.
3. For create operations, normalize the request and evaluate grants and CEL
   conditions before side effects.
4. For existing-resource operations, load minimal resource metadata, check exact
   ownership, then evaluate grants and CEL conditions.
5. Proceed to `controlservice` only after authorization succeeds.

### Streaming RPCs

Streaming APIs need an explicit first-frame authorization boundary:

- server-streaming APIs authorize before opening the stream
- `WriteSandboxFile` and `ExtractSandboxArchive` authorize after reading the
  init frame and before consuming file bytes
- `DialSandboxPort` authorizes the open frame before dialing the sandbox port
- execution and sandbox event streams authorize the target resource before
  sending any retained events

### List APIs

List APIs must not build a global response and filter it at the CLI. Filter at
the service or store boundary before results are returned:

- `ListSandboxes` returns only owned sandboxes
- `ListExecutions` returns only executions under owned sandboxes
- `ListSnapshots` returns only owned snapshots

### Health And Local Sockets

`/healthz` can remain unauthenticated.

Unix-socket behavior should be explicit:

- default: when `auth.required` is false, behavior remains local-trusted
- when `auth.required` is true, remote HTTP(S) requires OIDC
- add a separate `auth.require_unix_socket` option only if operators need to
  lock down local callers too

Do not silently treat remote HTTP as trusted. If auth is required and a caller
uses plain HTTP from a non-loopback endpoint, reject bearer-token auth rather
than accepting tokens over cleartext.

## Create-Time Constraints

`CreateSandbox` is the most important authorization point because it can use
host credentials, repository mirrors, backend resources, snapshots, and stage
caches.

Minimum constraints to expose to CEL or typed helpers:

- backend name
- repository remote URL
- repository commit SHA and branch hints
- snapshot source ID
- requested image ref and digest
- Docker requirement
- resource floors for vCPU, memory, and disk
- network default
- network allow hosts and ports by stage
- dependency and service block count
- cache reuse mode
- local changeset presence

The server must authorize repository input before `RepositoryStore` or gateway
credentials can satisfy a mirror hit. A local mirror hit for a private repository
must not bypass the same grant that would be required for a cold fetch.

## Content-Cache Boundary

Cleanroom uses `content-cache` in two different ways, and the authorization
model should treat them differently.

### Embedded `content-cache`

The embedded cache remains an implementation detail behind the Cleanroom host
gateway. It should not validate OIDC tokens, evaluate CEL, or understand
Cleanroom principals directly. Cleanroom should authenticate the control-plane
caller, stamp the sandbox owner, and register an enriched gateway scope when the
sandbox is provisioned.

Extend the gateway scope with owner and authorization metadata:

```go
type SandboxScope struct {
    SandboxID string
    Policy    *policy.CompiledPolicy
    Owner     ResourceOwner
    GatewayAuth GatewayAuthorization
}

type GatewayAuthorization struct {
    GitRepoPrefixes []string
    OCIRefs         []string
    FetchURLs       []string
}
```

The exact field names can change, but the invariant is that gateway wrappers
authorize the request before handing it to a `content-cache` protocol handler.
The protocol handler handles caching, upstream proxying, and singleflight. The
Cleanroom wrapper handles sandbox identity, principal-derived authorization,
policy allowlisting, and audit.

This matters for cached hits. A private Git or OCI object already present in the
cache must not be served just because the requesting sandbox has `github.com:443`
or a registry host in its network policy. The wrapper must authorize the
requested upstream resource for the sandbox owner on every request, including
cache hits, before the embedded handler can serve bytes.

For the first implementation:

- control-plane create grants produce a gateway authorization envelope for the
  sandbox
- gateway git wrappers check requested `host/owner/repo` against that envelope
  before calling `GitHandlerForHost`
- gateway OCI wrappers check the requested registry/image reference against
  that envelope before calling `OCIHandlerForPrefix`
- gateway Go, RubyGems, and fetch routes keep using sandbox network policy, and
  gain owner-aware constraints only when host credentials or private upstreams
  are added for those routes
- content-addressed blob storage can remain physically shared only if every
  route authorizes before serving cached data
- if a route cannot authorize a cached object precisely, partition that route's
  cache metadata by owner or disable that cached path under auth-enabled servers

Stage caches are different from transport caches. Dependency and service stage
caches restore filesystem state into a sandbox, so they should be owner-scoped
in Cleanroom's `cachestore` from the first auth-enabled version. Do not rely on
embedded `content-cache` boundaries to protect stage-cache reuse.

### Standalone `content-cache`

Standalone `content-cache` should not import Cleanroom's sandbox model. If it is
run as a shared service, it needs its own caller authorization around requested
upstream resources. That can reuse the same ideas:

- validate OIDC JWTs from callers
- map claims to route grants
- preflight cached Git/OCI hits before serving them
- keep upstream credentials host-side

The shared contract between Cleanroom and standalone `content-cache` should be a
small request-authorizer or credential-provider hook, not Cleanroom's
`Principal`, `SandboxScope`, or CEL schema. Cleanroom can pass a request-scoped
authorizer into embedded handlers when `content-cache` exposes that hook.
Standalone deployments can implement the same hook from their own config.

## Data Model Changes

Add owner metadata to in-memory state:

- `sandboxState.Owner`
- `executionState.Owner`
- `interactiveSessionState.Owner`

Add owner metadata to durable metadata stores:

- `snapshotstore.Record.OwnerPrincipalID`
- `snapshotstore.Record.OwnerScope`
- `cachestore.Record.OwnerPrincipalID`
- `cachestore.Record.OwnerScope`

Stage-cache lookup interfaces should include the caller owner, or cache keys
should include a stable owner prefix. Prefer explicit owner columns plus indexed
queries so future scope-sharing can be implemented without rewriting every cache
key.

Snapshot and cache SQLite stores need migrations that add nullable columns for
existing local metadata. When auth is enabled, missing owner metadata should deny
access rather than treating old records as globally readable.

## CLI And SDK Changes

Add a client auth option:

```bash
cleanroom exec --host https://server.example.com:7777 \
  --auth-token-env CLEANROOM_AUTH_TOKEN \
  -- echo hello
```

Useful environment and config hooks:

- `CLEANROOM_AUTH_TOKEN`
- `--auth-token-file` for local short-lived token files
- `--auth-token-env` for explicit env selection in scripts

Avoid printing tokens in debug logs, trace attributes, errors, or startup
headers.

Add an operator diagnostic command:

```bash
cleanroom auth check \
  --token-file /tmp/token.jwt \
  --action sandbox.create \
  --request create-sandbox.json
```

The command should explain which issuer, binding, grant, ownership rule, or CEL
condition allowed or denied the request. This is important because expression
policies become hard to operate without a local explain path.

## Observability And Audit

Every auth decision should emit structured audit data:

- decision: allow or deny
- principal ID
- issuer
- subject
- scope
- action
- resource kind and ID when available
- owner principal ID and scope when available
- matching binding and grant name when available
- deny reason code

Do not log token strings or secret claim values. Logs should prefer stable
reason codes over raw expression errors, with detailed expression diagnostics
available only through local config validation or `auth check`.

Suggested deny reason codes:

```text
auth_missing
auth_invalid_token
auth_untrusted_issuer
auth_audience_mismatch
auth_no_binding
auth_no_grant
auth_condition_false
auth_condition_error
auth_owner_mismatch
auth_resource_missing_owner
auth_insecure_transport
```

## Delivery Strategy

### Slice 1: Auth Engine And Config Validation

Scope:

- Add runtime auth config types for OIDC issuers and policy file path.
- Add an internal auth package for OIDC token validation with fake-JWKS tests.
- Add binding evaluation from token claims to `Principal`.
- Add typed grant parsing and CEL compilation.
- Add an internal authorizer that can answer allow/deny for normalized requests.
- Add `cleanroom auth check` against local config and a JSON request fixture.
- Keep the control server behavior unchanged until enforcement lands.

Definition of done: invalid issuers, audiences, signatures, expiry, malformed
bindings, unknown CEL fields, and condition failures are covered by unit tests,
and operators can run a local auth check without starting a server.

### Slice 2: Server Enforcement, Exact Ownership, And Create Constraints

Scope:

- Add client bearer-token support.
- Add server authentication middleware for ConnectRPC handlers.
- Require auth for configured remote HTTP(S) endpoints.
- Add normalized request views for repository, policy, resources, image, Docker,
  network, and snapshot source attributes.
- Evaluate create-time grants and CEL conditions before repository mirror,
  snapshot restore, stage-cache lookup, or backend work.
- Register owner and gateway authorization metadata with the sandbox gateway
  scope.
- Authorize Git and OCI gateway requests against the sandbox owner's gateway
  authorization envelope before embedded `content-cache` handlers can serve
  cached or upstream responses.
- Stamp owner metadata on new sandboxes, executions, interactive sessions,
  snapshots, and cache records.
- Enforce exact-owner access on every sandbox, execution, snapshot, file,
  archive, extract, port, and stream RPC.
- Filter list APIs by owner.
- Partition stage-cache lookup and publication by owner.
- Deny auth-enabled access to pre-existing snapshot or cache records that have no
  owner metadata.
- Add docs for OIDC setup and ownership behavior.

Definition of done: with auth enabled, two valid principals can use the same
server and cannot list, get, execute in, attach to, copy from, snapshot, restore,
port-forward, stream, or warm-cache-hit each other's resources. A principal can
create sandboxes only within its configured repository, backend, image, resource,
and network constraints, and attempts outside those constraints fail before any
host-side fetch, restore, cache lookup, or VM provisioning begins.

### Slice 3: Runtime Hardening And Audit

Scope:

- Add fail-closed handling for CEL runtime errors.
- Add audit events and denial reason codes.
- Reject bearer tokens over insecure non-loopback HTTP when auth is required.
- Add transport and token redaction tests.

Definition of done: auth failures are observable without leaking token material,
transport behavior is fail-closed for bearer tokens, and every deny path returns
stable reason codes that can be explained by `cleanroom auth check`.

### Slice 4: Controlled Sharing

Scope:

- Decide whether scope-level sharing is needed for bot pools.
- Add explicit grant actions for sharing, if needed.
- Support scope-level cache reuse only when both producer and consumer grants
  allow it.
- Add snapshot publish or import semantics only after same-principal isolation is
  proven.

Definition of done: sharing is opt-in, audited, and impossible to confuse with
default exact-principal ownership.

## Verification

Unit tests:

- OIDC validator rejects bad issuer, audience, signature, expiry, not-before,
  missing `kid`, and unsupported algorithms.
- JWKS cache refreshes on key rotation and fails closed on fetch errors.
- Binding evaluation derives stable principal IDs from fake claims.
- CEL expressions compile at config load and deny on runtime errors.
- Grant evaluation requires both action/resource match and condition success.
- Create-request normalization exposes only stable fields.

Control server tests:

- unauthenticated remote requests fail when auth is required
- authenticated principal A cannot direct-get principal B's sandbox
- list APIs hide other principals' resources
- `CreateExecution` denies cross-owner sandbox IDs
- file read/write/archive/extract denies cross-owner sandbox IDs
- `DialSandboxPort` denies cross-owner sandbox IDs
- execution stream and sandbox event stream deny before sending retained events
- snapshot create inherits owner and restore requires same owner
- interactive attach only returns a session token after owner authorization
- stage-cache lookup does not hit records owned by another principal
- embedded Git and OCI cache hits are denied when the sandbox owner is not
  authorized for the requested upstream repo or image
- auth-enabled access to ownerless old metadata fails closed

CLI and integration tests:

- `CLEANROOM_AUTH_TOKEN` attaches bearer auth to ConnectRPC requests
- `auth check` reports allow and deny decisions with binding and grant names
- tokens are absent from logs, errors, traces, and startup output
- HTTPS with valid token succeeds; insecure non-loopback HTTP with bearer token
  fails when auth is required

Runtime smoke test:

- start a local HTTPS test server with two signed tokens
- create one sandbox per token
- prove cross-principal list/get/execute/file/snapshot attempts are denied
- prove same-principal operations still work

## Resolved Decisions

- Start with OIDC JWT identity rather than SPIFFE or mTLS.
- Keep authorization config in runtime/server config, not repository policy.
- Use Cleanroom-native typed actions and resources.
- Use CEL only as a conditional expression language inside typed grants.
- Stamp ownership server-side and reject client-provided ownership.
- Enforce exact-principal ownership before adding scope-level sharing.
- Partition stage-cache records by owner in the first auth-enabled version.
- Treat missing owner metadata as deny when auth is enabled.

## Deferred Work

- SPIFFE or client-certificate identity provider support.
- OAuth token exchange for delegated child-bot credentials.
- External OPA or Cedar integration for operators that already run those
  systems.
- Relationship-based sharing across teams, bot pools, or repositories.
- Admin APIs for listing principals, grants, and audit decisions.
- Persistent sandbox and execution stores beyond current retained server state.

## Open Questions

- Should unix-socket callers remain local-trusted when remote auth is required?
  Recommended default: yes for the first implementation, with an explicit
  `auth.require_unix_socket` hardening option later.
- Which OIDC issuer should the examples use first?
  Recommended default: GitHub Actions because it is widely available and has
  useful repository claims, while keeping the implementation generic.
- Should stage-cache sharing ever be allowed at repo scope?
  Recommended default: no for the first auth-enabled version; exact-principal
  cache partitioning is simpler and avoids leaking private dependency outputs.

## Key Learnings From Pressure-Testing

Partial enforcement is more dangerous than no enforcement. An auth-enabled
server must not protect only list APIs while leaving direct `Get`, stream, file,
snapshot, or port APIs unchecked. The plan therefore requires ownership checks
for every resource-bearing RPC before enforcement is considered done.

Repository mirrors and warm stage caches can bypass upstream fetch paths if
authorization only happens around cold network work. The plan therefore requires
create-time authorization before mirror use and owner partitioning for cache
lookup and publication.

Embedded `content-cache` can also bypass upstream checks if Cleanroom authorizes
only cold upstream requests. The plan therefore keeps Cleanroom's gateway
wrappers responsible for owner and policy checks on every request before cached
bytes can be served.

CEL is useful for constraints, but it should not define the resource model.
Keeping typed actions, typed resources, and exact-owner checks outside CEL makes
authorization easier to audit and keeps expression bugs from widening access.

Interactive QUIC remains a bearer-session-token transport after
`AttachExecution`. The first implementation should keep those tokens
short-lived and issue them only after owner authorization. Stronger binding of
the QUIC leg to the authenticated principal can be a follow-up if the bearer
session token becomes an unacceptable risk.
