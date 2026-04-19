# Host gateway

The Cleanroom server runs a shared host gateway that mediates sandbox access to
selected external services. The current implementation embeds
`github.com/buildkite/content-cache` behind the gateway for cache-backed Git
smart-HTTP and OCI registry routes, while keeping sandbox identity, policy
enforcement, and credential resolution in Cleanroom's own `internal/gateway`
layer.

Currently supported on the `firecracker` and `darwin-vz` backends.

Inside sandboxes, the shared gateway is exposed at
`http://gateway.cleanroom.internal:8170` by default.

## Route status

| Path | Status | Purpose |
|------|--------|---------|
| `/git/` | Implemented | Cache-backed Git smart-HTTP route. `.git` remotes use embedded `content-cache`; non-cacheable paths fall back to Cleanroom's mirror-backed proxy. |
| `/registry/` | Implemented | Cache-backed OCI Distribution route for allowlisted registries. |
| `/rubygems/` | Implemented | Cache-backed RubyGems route for Bundler mirror traffic to `rubygems.org`. |
| `/secrets/` | Reserved | Not implemented yet. |
| `/meta/` | Reserved | Not implemented yet. |

## Sandbox identity

On `firecracker`, the gateway identifies the sandbox from the guest source IP.
On `darwin-vz`, guests share NAT, so requests use the
`X-Cleanroom-Scope-Token` header. The gateway normally restricts that header to
configured gateway-source prefixes, but falls back to allowing it from any
source if gateway-host resolution is unavailable.

## Git proxy

Cleanroom rewrites clone URLs to the host gateway when the target host is in
`sandbox.network.allow`. Clone commands run unchanged inside the sandbox:

```bash
cleanroom exec -- git clone https://github.com/org/repo.git
```

The gateway resolves the target host from the request path, validates it against
the sandbox's compiled policy, and proxies the git smart-HTTP protocol upstream.
For `.git` smart-HTTP routes, the request is handed to embedded
`content-cache`; for non-`.git` paths, the gateway falls back to Cleanroom's
mirror-backed proxy. Guest-side git rewrites target
`gateway.cleanroom.internal` rather than backend-specific IP addresses.

When you want to restrict which Git hosts use the cache layer without denying
other policy-allowed Git traffic, set `gateway.git.cache_hosts` in runtime
config. Hosts outside that list fall back to the mirror-backed proxy.

On `darwin-vz`, sandbox identity is carried with the
`X-Cleanroom-Scope-Token` request header because guests use shared NAT rather
than unique source IPs.

Allowed host example (from this repo's policy):

```bash
cleanroom exec -- git ls-remote https://github.com/buildkite/cleanroom.git HEAD
```

Denied host example (not in `sandbox.network.allow`):

```bash
cleanroom exec -- git ls-remote https://gitlab.com/gitlab-org/gitlab.git HEAD
```

## Registry route

`/registry/` is backed by `content-cache`'s OCI handlers rather than a generic
HTTP forward proxy. Cleanroom resolves the registry prefix from the request
path, maps it to an upstream registry, checks the mapped host and port against
the sandbox policy, and only then allows the upstream fetch.

Current scope:

- OCI pull-style `GET` and `HEAD` requests
- built-in prefix mapping for Docker Hub (`docker.io` ->
  `https://registry-1.docker.io`)
- additional registry-prefix mappings configured inside the gateway

Not wired yet:

- guest-wide package-manager rewrites to `/registry/`
- lockfile enforcement
- non-OCI package-manager protocol handling

## RubyGems route

`/rubygems/` is backed by `content-cache`'s RubyGems handler. It serves the
RubyGems Compact Index (`/versions`, `/info/<gem>`, `/names`), legacy specs
metadata, and gem downloads while applying the sandbox allowlist to the
upstream `rubygems.org` host before any upstream request is made.

Current scope:

- Bundler mirror traffic for the default `https://rubygems.org` source
- Compact Index, legacy specs, and gem download requests
- guest-side Bundler mirror environment injection when `rubygems.org` is
  allowlisted

Not wired yet:

- generic `gem sources` rewrites for arbitrary RubyGems registries
- lockfile enforcement
- private RubyGems registry authentication flows

## Credentials

Host-side credentials are provided via environment variables:

| Variable | Purpose |
|----------|---------|
| `CLEANROOM_GITHUB_TOKEN` | GitHub authentication |
| `CLEANROOM_GITLAB_TOKEN` | GitLab authentication |

Credentials are injected into upstream requests by the gateway. They are never
exposed to the guest environment. The same host-side credential provider chain
is used by the embedded `content-cache` upstream clients.

## Configuration

The gateway listens on `:8170` by default. Use `--gateway-listen` to change:

```bash
cleanroom serve --gateway-listen :0    # ephemeral port
```
