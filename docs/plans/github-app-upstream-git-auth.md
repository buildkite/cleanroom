# GitHub App Upstream Git Auth Plan

**Spec reference:** `docs/spec.md` section 6.2, `docs/gateway.md`
**Related plan:** `docs/plans/layered-caching.md`
**Status:** Active
**Last reviewed:** 2026-05-22

## Summary

Add GitHub App installation-token support for upstream Git Smart HTTP requests
without exposing credentials to sandboxes or cache clients.

The ownership boundary is the core design point:

- `content-cache` should own the standalone upstream Git auth mechanism for use
  outside Cleanroom.
- Cleanroom should own sandbox identity, policy, stage-scoped egress, and the
  host-side credential provider it passes into embedded `content-cache`.
- Neither project should inject GitHub credentials into the guest command
  environment.

In practice this means `content-cache` gets a dynamic Git auth provider that can
mint and cache GitHub App installation tokens for standalone proxy deployments.
Cleanroom can either keep using its existing gateway credential provider chain or
wrap the same token-source package when it wants GitHub App credentials for
sandboxed repository fetches.

## Problem

Cleanroom already keeps upstream Git credentials on the host. Guest Git commands
receive scoped URL rewrite config, and upstream requests get credentials only
inside the host gateway. The embedded `content-cache` path also receives a
Cleanroom `CredentialProvider`, so cache-backed Git requests can be authenticated
without putting tokens in the sandbox.

That does not solve standalone `content-cache` deployments outside Cleanroom.
Standalone `content-cache` currently has static Git route credentials in its
credentials file, and its Git upstream client applies Basic auth from those
route records. That model works for PATs and fixed service credentials, but it is
the wrong lifecycle for GitHub App installation tokens:

- installation tokens expire after a short lifetime
- tokens must be minted with a GitHub App private key and installation ID
- the best token scope depends on the requested repository or route
- token values must not be logged, stored in repo config, or exposed to clients

If this is implemented only in Cleanroom, `content-cache` users outside Cleanroom
still need an external credential sidecar or long-lived PAT. If it is implemented
only as a `content-cache` server feature with no authorization model, a shared
cache can become a confused-deputy Git proxy for every repository visible to the
GitHub App installation.

## Goals

- Support GitHub App installation tokens for Git Smart HTTP upstream requests.
- Keep token minting and token injection host-side.
- Make standalone `content-cache` useful without Cleanroom.
- Keep Cleanroom policy and sandbox identity out of `content-cache`.
- Preserve current static Basic auth and unauthenticated public Git behavior.
- Fail closed when token minting, route matching, or caller authorization is
  ambiguous.
- Make token refresh and cache behavior testable without talking to GitHub in
  unit tests.

## Non-goals

- Do not add guest environment secret injection.
- Do not implement the reserved `/secrets/` gateway route.
- Do not add Git push support through the gateway or cache.
- Do not make `cleanroom.yaml` carry host credential configuration.
- Do not require GitHub App auth for public repositories.
- Do not solve GHCR, Packages, release asset, or API auth in the first slice.
- Do not make `content-cache` understand Cleanroom sandbox IDs, stage policy, or
  backend details.
- Do not support GitHub Enterprise Server in the first implementation.

## Current State

Cleanroom has the right host-side boundary already:

- `internal/gateway/credentials.go` defines `CredentialProvider` as a resolver
  from upstream Git remote URL to an HTTP `Authorization` header.
- `internal/cli/serve.go` builds a credential chain from environment variables
  and host `git credential fill`.
- `internal/gateway/contentcache.go` passes that provider into embedded
  `content-cache`.
- `internal/gateway/contentcache_types.go` injects credentials into upstream
  HTTP requests made by embedded `content-cache`.
- `internal/gateway/mirror.go` uses remote-scoped Git `extraHeader` config for
  host-side mirror clone/fetch commands.

Standalone `content-cache` has adjacent but static machinery:

- `credentials.Credentials.Git.Routes` configures per-prefix Git auth records.
- `server/http.go` converts those records into `protocol/git.Route` values.
- `protocol/git.Upstream` supports static `WithBasicAuth`.
- Inbound static token or OIDC auth authorizes by protocol, not by Git
  repository path.

The gap is dynamic per-request upstream authentication plus a safe caller
authorization story for shared standalone deployments.

Progress as of 2026-05-22:

- `content-cache` has landed dynamic Git auth, GitHub App route config,
  repo-scoped installation token minting, cached upload-pack auth preflight,
  and fail-closed trusted single-tenant gating.
- `content-cache` has exported the GitHub App auth API URL, HTTP client, and
  clock options so downstream packages can wrap the token source in tests.
- The active Cleanroom slice wraps that content-cache token source as a
  `gateway.CredentialProvider` configured from host runtime environment
  variables, ahead of static env tokens and host `git credential fill`.

## Ownership Model

| Responsibility | Owner | Notes |
| --- | --- | --- |
| Standalone GitHub App upstream auth | `content-cache` | Needed when `content-cache` is run outside Cleanroom. |
| Generic dynamic Git auth interface | `content-cache` | Belongs beside `protocol/git.Upstream` and route selection. |
| GitHub App token mint/cache implementation | Shared or `content-cache` package | Cleanroom can wrap it later without using standalone server config. |
| Sandbox policy and stage egress | Cleanroom | Must not move into `content-cache`. |
| Guest Git URL rewrite | Cleanroom | Still only rewrites allowed remotes to the host gateway. |
| Embedded cache credential injection | Cleanroom | Keep using `gateway.CredentialProvider`. |
| Standalone caller auth and resource auth | `content-cache` | Required if one cache serves multiple trust domains. |

## Target Model

### Standalone `content-cache`

The standalone server should support dynamic Git auth in the Git route table.
The exact schema can be refined in `content-cache`, but the shape should be
route-scoped and explicit:

```json
{
  "git": {
    "routes": [
      {
        "match": { "repo_prefix": "github.com/buildkite/" },
        "github_app": {
          "app_id": "{{ env \"GITHUB_APP_ID\" | json }}",
          "installation_id": "{{ env \"GITHUB_INSTALLATION_ID\" | json }}",
          "private_key": "{{ file \"/run/secrets/github-app.pem\" | json }}",
          "token_scope": "requested_repo"
        }
      },
      {
        "match": { "any": true }
      }
    ]
  }
}
```

The first implementation can require explicit `installation_id` per route.
Automatic installation discovery from `owner/repo` is useful later, but it adds
API calls, negative caching, and GitHub Enterprise Server questions that are not
needed to prove the model.

The provider should set Git HTTP auth as Basic auth with username
`x-access-token` and the installation token as the password. Token values should
never appear in request logs, error messages, metrics labels, route dumps, or
debug output.

### Cleanroom

Cleanroom should not consume the standalone `content-cache` credentials-file
schema. Its runtime ownership remains:

- repository policy says which Git hosts are allowed
- runtime config or host environment says how the host gateway authenticates
- guest commands see Git URL rewrite config, not credentials
- upstream auth is attached in the host gateway or the embedded cache HTTP client

If Cleanroom needs GitHub App auth, add a Cleanroom-side
`gateway.CredentialProvider` that wraps the shared GitHub App token source and
returns an Authorization header for `https://github.com/<owner>/<repo>.git`.
That provider can sit before the existing env and `git credential fill` fallback
in the gateway credential chain.

The first Cleanroom implementation uses explicit host environment variables:

```console
CLEANROOM_GITHUB_APP_ID=12345
CLEANROOM_GITHUB_APP_INSTALLATION_ID=67890
CLEANROOM_GITHUB_APP_PRIVATE_KEY_FILE=/run/secrets/github-app.pem
CLEANROOM_GITHUB_APP_REPO_PREFIXES=buildkite/,example-org/private-repo
```

`CLEANROOM_GITHUB_APP_PRIVATE_KEY` can be used instead of the file variable when
the host environment can safely carry a PEM value. `CLEANROOM_GITHUB_APP_REPO_PREFIXES`
is required so a configured GitHub App does not claim every `github.com` remote;
repositories outside those prefixes continue through the remaining host
credential providers. These variables are runtime gateway configuration and must
not be copied into the guest or `cleanroom.yaml`.

## Auth And Safety Invariants

- Upstream credentials stay host-side.
- Credential config belongs to runtime or cache server config, not committed
  repository policy.
- Token cache keys must include at least GitHub host, app ID, installation ID,
  requested repository scope, and requested permissions.
- Tokens should be refreshed before expiry and singleflighted to avoid request
  bursts minting many identical tokens.
- A token mint failure should fail the upstream Git request, not fall back to an
  unauthenticated request for a private route.
- A configured GitHub App route should not silently match a different host.
- Cleanroom gateway policy still decides whether a sandbox may contact
  `github.com:443`.
- Standalone `content-cache` must document whether a deployment is trusted
  single-tenant or has repo-level caller authorization.

## Standalone Authorization Model

GitHub App auth changes the risk profile of standalone `content-cache`. A static
PAT route already carries this risk, but GitHub Apps make broad installation
access more attractive and easier to centralize.

For single-tenant deployments, the first version can treat the cache as a trusted
component for that tenant only when the server config says so explicitly. In
that mode it can rely on:

- `--git-allowed-hosts`
- explicit Git route prefixes
- inbound auth that limits access to the cache service
- no unauthenticated catch-all route that receives app credentials

For shared deployments, protocol-level OIDC permission is not enough. A caller
with `git` permission can request any Git path unless the server also checks the
requested `host/owner/repo` against caller claims or policy. GitHub App auth
must not be advertised as safe for shared standalone deployments until this check
exists:

```json
{
  "permissions": ["git"],
  "git": {
    "repo_prefixes": ["github.com/buildkite/"]
  }
}
```

This resource authorization belongs in standalone `content-cache`. Cleanroom
already has sandbox-scoped policy enforcement around embedded cache requests.

Startup should fail closed for ambiguous standalone configurations. If a GitHub
App route is configured and repo-level caller authorization is not configured,
the server should require an explicit trusted single-tenant acknowledgement.
That keeps the dangerous state visible instead of making "protocol-level auth
plus broad app installation" look like a secure multi-tenant deployment.

## Delivery Strategy

### Slice 1: Dynamic Git auth interface in `content-cache`

Status: landed in `content-cache`.

Add a provider abstraction under `protocol/git` and convert static Basic auth to
use it.

Candidate shape:

```go
type AuthProvider interface {
    AuthenticateGitRequest(ctx context.Context, repo RepoRef, req *http.Request) error
}
```

`WithBasicAuth` can become a small static provider. `Upstream.FetchInfoRefs` and
`Upstream.FetchUploadPack` should call the provider after request construction
and before the HTTP client executes the request.

Definition of done:

- static Basic auth behavior remains unchanged
- auth provider errors fail the request before upstream I/O
- tests cover `info/refs` and `git-upload-pack`
- logs do not include Authorization headers or token-like values

### Slice 2: GitHub App token source and route config in `content-cache`

Status: landed in `content-cache`, with downstream test hooks exported.

Add a GitHub App auth provider and wire it from the credentials file.

Implementation notes:

- parse app ID, installation ID, private key, and token scope from the
  credentials template output
- mint JWTs from the private key
- exchange JWTs for installation tokens through the GitHub App API
- cache installation tokens until shortly before expiry
- use singleflight for concurrent misses
- set Git request auth as `x-access-token:<installation-token>`
- keep GitHub API and clock injectable for tests
- fail startup unless GitHub App routes either have repo-level caller
  authorization configured or the server is explicitly marked trusted
  single-tenant

Definition of done:

- unit tests cover token minting, caching, refresh-before-expiry, and failures
- an HTTP test upstream proves the Git request receives the expected Basic auth
- tests prove `token_scope: "requested_repo"` sends a repo-constrained token
  request and that token cache keys include the canonical `owner/repo`
- tests prove GitHub App route startup fails without repo-level caller
  authorization or explicit trusted single-tenant mode
- credentials redaction is covered by tests where practical
- docs show a minimal credentials-file example

### Slice 3: Standalone repo authorization hardening

Status: deferred until there is a shared standalone `content-cache` deployment
target. Trusted single-tenant mode is the supported standalone path for now.

Implement the shared-deployment authorization path.

Scope:

- extend OIDC trust policies with optional Git repo-prefix constraints
- enforce those constraints in the `/git/` handler before selecting upstream auth
- add tests proving a caller with `git` permission but the wrong repo prefix is
  denied before upstream auth is minted
- document that trusted single-tenant mode is the only supported alternative
  until these checks are configured

### Slice 4: Cleanroom integration

Status: active.

Cleanroom wraps the standalone token source through its existing gateway
credential provider chain.

Potential Cleanroom work:

- add a `gateway.GitHubAppCredentialProvider` that uses the shared token source
- configure it from runtime config or environment, not `cleanroom.yaml`
- place it before env-token and host-git-credential fallback only when explicitly
  configured, and only for explicit repo prefixes
- distinguish "no credential configured" from "a configured credential provider
  failed"; provider failure must fail the upstream Git request instead of falling
  through to unauthenticated fetch
- keep embedded `content-cache` receiving the existing `CredentialProvider`
- wire the credential provider into embedded `content-cache` Git auth preflight,
  not only the HTTP transport, so cached upload-pack hits still fail closed when
  GitHub App token minting fails
- add tests proving guest command env does not contain Authorization headers,
  private keys, installation tokens, or GitHub App config

This slice is optional for the standalone use case. Cleanroom already has a
working provider interface and should not block `content-cache` from supporting
outside-Cleanroom deployments.

## Verification

`content-cache`:

- `go test ./protocol/git ./credentials ./server`
- provider unit tests with fake clocks and fake GitHub API responses
- handler tests for `info/refs` and `git-upload-pack` auth behavior
- OIDC/resource-auth tests for shared-deployment authorization
- a local Smart HTTP integration test where upstream rejects requests without the
  expected GitHub App-derived auth

Cleanroom:

- `go test ./internal/gateway ./internal/cli`
- tests that embedded `content-cache` still receives host-side credentials
- tests that a configured Cleanroom credential provider failure fails closed
  rather than issuing an unauthenticated upstream request
- tests that repository bootstrap and execution command snapshots do not embed
  Authorization headers or app secrets
- optional smoke test with a fake GitHub App provider and sandbox policy allowing
  `github.com:443`

Manual validation with real GitHub credentials should be gated and explicit:

```console
CONTENT_CACHE_GITHUB_APP_TEST=1 go test ./protocol/git -run GitHubApp
```

The test should use a disposable private repository or a read-only test
installation with Contents read permission only.

## Key Learnings From Pressure-Testing

- A static credentials template is not enough for GitHub App tokens because token
  lifetime and repo scope are request-time concerns.
- Putting the whole feature in Cleanroom would leave standalone `content-cache`
  users on PATs or custom sidecars.
- Putting Cleanroom sandbox policy in `content-cache` would blur the transport
  cache boundary and make standalone deployments inherit Cleanroom-specific
  concepts.
- Broad installation tokens can silently widen repository access. The plan now
  requires either explicit trusted single-tenant mode or repo-level caller
  authorization before GitHub App routes can be used.
- The smallest useful first slice is a generic dynamic Git auth interface in
  `content-cache`, not the GitHub App provider itself.
- GitHub Enterprise Server support is not needed for the first implementation;
  the dotcom path should stay smaller and prove the token/auth boundary first.

## Resolved Decisions

- Standalone GitHub App upstream auth belongs in `content-cache`.
- Cleanroom keeps policy enforcement and guest credential isolation.
- GitHub App credentials should be runtime/cache-server configuration, not
  repository policy.
- The first provider should require explicit installation configuration rather
  than auto-discovering installations.
- Push support remains out of scope.
- GitHub Enterprise Server support is deferred until there is a concrete
  deployment target.
- Cleanroom wraps `content-cache/protocol/git.NewGitHubAppAuth` rather than
  copying token minting code into Cleanroom.
- Cleanroom GitHub App configuration is host runtime environment, not
  repository policy.
- Cleanroom requires explicit GitHub App repo prefixes; an app configuration
  must not claim all `github.com` remotes and block fallback credentials for
  unrelated repositories.
- Standalone repo-level OIDC authorization is deferred until a shared
  standalone `content-cache` deployment needs it.

## Open Questions

None for the current Cleanroom integration slice.
