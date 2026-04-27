# Host gateway

The Cleanroom server runs a shared host gateway that mediates sandbox access to
selected external services. The current implementation embeds
`github.com/buildkite/content-cache` behind the gateway for cache-backed Git
smart-HTTP, OCI registry, Go module proxy, RubyGems, and immutable download
routes, while keeping sandbox identity, policy enforcement, and credential
resolution in Cleanroom's own `internal/gateway` layer.

Currently supported on the `firecracker` and `darwin-vz` backends.

Inside sandboxes, the shared gateway is exposed at
`http://gateway.cleanroom.internal:8170` by default.

## Route status

| Path | Status | Purpose |
|------|--------|---------|
| `/v2/` | Implemented | Docker Hub-compatible pull-through mirror endpoint for guest `dockerd`. |
| `/git/` | Implemented | Cache-backed Git smart-HTTP route. `.git` remotes use embedded `content-cache`; non-cacheable paths fall back to Cleanroom's mirror-backed proxy. |
| `/registry/` | Implemented | Cache-backed OCI Distribution route for allowlisted registries. |
| `/goproxy/` | Implemented | Cache-backed Go module proxy route. Also serves mirrored checksum database requests under `/goproxy/sumdb/`. |
| `/rubygems/` | Implemented | Cache-backed RubyGems route for Bundler mirror traffic to `rubygems.org`. |
| `/fetch/` | Implemented | Cache-backed immutable download route for configured upstream hosts such as `dl.google.com`. |
| `/secrets/` | Reserved | Not implemented yet. |
| `/meta/` | Reserved | Not implemented yet. |

## Sandbox identity

On `firecracker`, the gateway identifies the sandbox from the guest source IP.
On `darwin-vz`, the file-handle gateway bridge forwards requests with the
`X-Cleanroom-Scope-Token` header. The gateway accepts scope-token requests from
loopback by default, which covers the host-local file-handle bridge. When a
gateway host is configured, Cleanroom derives additional trusted IPv4 `/24` or
IPv6 `/64` prefixes from that host or its resolved addresses. If gateway-host
resolution fails or returns no addresses, Cleanroom falls back to loopback-only
scope-token trust rather than accepting scope tokens from arbitrary sources.

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
- additional registry-prefix mappings configured via
  `gateway.oci.registries` in runtime config
- custom registry keys must be real registry hosts, not arbitrary aliases

Example runtime config:

```yaml
gateway:
  oci:
    registries:
      ghcr.io: https://ghcr.io
      registry.internal:5000: https://registry.internal:5000
```

Not wired yet:

- guest-wide package-manager rewrites to `/registry/`
- lockfile enforcement
- non-OCI package-manager protocol handling

## Docker Hub mirror

`/v2/` exposes a Docker Hub-compatible mirror endpoint backed by the same
embedded OCI cache. When guest Docker service support is enabled and the OCI
cache route is live, Cleanroom starts `dockerd` with this gateway endpoint as a
Docker Hub registry mirror.

Current scope:

- guest `dockerd` pull-through caching for Docker Hub images
- pull-style `GET` and `HEAD` requests only
- mirror traffic routed to the same `docker.io` -> `registry-1.docker.io`
  upstream mapping used by `/registry/`

Not wired yet:

- guest daemon rewrites for non-Docker-Hub registries
- Docker push or other write operations through the mirror

## Go module proxy route

`/goproxy/` is backed by `content-cache`'s Go module proxy and mirrored checksum
database handlers. It serves `GOPROXY` requests for `@v/list`, `.info`, `.mod`,
and `.zip`, while also answering mirrored checksum-database requests under
`/goproxy/sumdb/`.

Current scope:

- guest-side `GOPROXY` environment injection when `proxy.golang.org` is
  allowlisted
- Go module metadata and zip fetches through the shared host gateway
- mirrored checksum-database requests when `sum.golang.org` is allowlisted
- host-side redirect validation for proxy redirects such as
  `storage.googleapis.com`

Not wired yet:

- lockfile-derived Go module allowlists
- private module or private checksum-database authentication flows
- non-default upstream Go proxy or sumdb configuration

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

## Immutable fetch route

`/fetch/` is backed by `content-cache`'s immutable download handler. Cleanroom
uses it for guest-side tool downloads that are expressed as direct HTTPS
artifacts rather than registry APIs.

Current scope:

- guest-side `MISE_GO_DOWNLOAD_MIRROR` injection when `dl.google.com` is
  allowlisted
- cache-backed Go SDK downloads from `dl.google.com/go/...`
- host-side redirect validation and sandbox allowlist enforcement for configured
  fetch hosts

Not wired yet:

- broader guest-side tool download rewrites beyond the configured fetch hosts
- lockfile-aware artifact allowlists
- authenticated private artifact downloads

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
