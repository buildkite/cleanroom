# Sandbox Wildcard Exposure Plan

Builds on docs/plans/sandbox-port-exposure.md

**Related:** `docs/plans/sandbox-port-exposure.md`
**Status:** Proposed
**Last reviewed:** 2026-05-05

## Summary

Allow HTTPS exposures to register wildcard nested-host routes under
`cleanroom.localhost`, while forwarding the original external host, scheme, and
port to the upstream app.

This keeps the current CLI shape but changes nested-host support to be
wildcard-only. For example:

```bash
cleanroom exec \
  --expose-https buildkite:3000 \
  --expose-https '*.buildkite:3000' \
  -- npm run dev
```

should support:

- `https://buildkite.cleanroom.localhost:8143`
- `https://api.buildkite.cleanroom.localhost:8143`
- `https://agent.buildkite.cleanroom.localhost:8143`

without requiring explicit registrations such as `api.buildkite:3000` or
`agent.buildkite:3000`.

## Problem

The current HTTPS exposure layer only accepts a single DNS label as the route
name. A named exposure such as:

```bash
cleanroom expose --in <sandbox-id> --expose-https buildkite:3000
```

creates exactly one route:

- `buildkite.cleanroom.localhost`

That is not enough for local apps like Buildkite that expect additional hosts
such as:

- `api.buildkite.cleanroom.localhost`
- `agent.buildkite.cleanroom.localhost`

Separately, the current HTTPS reverse proxy path does not provide trusted
`X-Forwarded-Host`, `X-Forwarded-Proto`, or `X-Forwarded-Port` headers to the
upstream app. Apps that generate absolute redirects therefore fall back to
their internal request view and can emit incorrect redirect URLs.

## Goals

- Keep the current `--expose-https [name:]<guest-port>` CLI surface.
- Support exact single-label names such as `buildkite`.
- Support wildcard nested-host names such as `*.buildkite`.
- Do not support explicit dotted registrations such as `api.buildkite`.
- Preserve the original client `Host` header when proxying to the guest.
- Set trusted forwarded headers so upstream apps can build correct absolute
  URLs.
- Keep the shared local exposure certificate inputs consistent between HTTPS
  exposure startup and `cleanroom dns install`.
- Avoid backend-specific behavior; this is a client-side exposure change only.

## Non-Goals

- Do not support explicit dotted exact names such as `api.buildkite`.
- Do not add a separate wildcard-specific CLI flag.
- Do not add broad suffix matching beyond the requested wildcard shape.
- Do not redesign DNS installation or macOS trust mechanics beyond making them
  use the same shared exposure certificate inputs.
- Do not store wildcard alias state in sandbox metadata.

## Proposed Behavior

Each `--expose-https` entry continues to register one route, but nested hosts
are exposed only through wildcard names.

For example:

```bash
cleanroom expose --in <sandbox-id> \
  --expose-https buildkite:3000 \
  --expose-https '*.buildkite:3000'
```

should register:

- exact `buildkite.cleanroom.localhost`
- wildcard `*.buildkite.cleanroom.localhost`

and yield this behavior:

- `buildkite.cleanroom.localhost` matches only the exact `buildkite` route
- `api.buildkite.cleanroom.localhost` matches `*.buildkite`
- `agent.buildkite.cleanroom.localhost` matches `*.buildkite`
- `buildkite.cleanroom.localhost` does not match `*.buildkite`
- `foo.bar.buildkite.cleanroom.localhost` does not match `*.buildkite`

If deeper wildcard matching is ever needed, that should be requested with a
separate wildcard such as `*.bar.buildkite`, not via explicit dotted exact
hosts.

## Proxy Semantics

The HTTPS proxy should forward enough trusted request context for common web
apps and reverse proxies to behave correctly.

For an incoming request such as:

```text
GET / HTTP/2
Host: api.buildkite.cleanroom.localhost:8143
```

the request that reaches the guest service should preserve:

- `Host: api.buildkite.cleanroom.localhost:8143`
- `X-Forwarded-Host: api.buildkite.cleanroom.localhost:8143`
- `X-Forwarded-Proto: https`
- `X-Forwarded-Port: 8143`
- `X-Forwarded-For: <client-ip>`

If the inbound host omits an explicit port, set `X-Forwarded-Port` from the
default for the inbound scheme (`443` for TLS, `80` otherwise).

## Design

### Route Naming

Expand the current HTTPS exposure validator from a single DNS label to one of
these two shapes:

- an exact single DNS label such as `buildkite`
- a leading wildcard plus dotted suffix such as `*.buildkite`

Valid examples:

- `buildkite`
- `*.buildkite`
- `*.foo.bar.buildkite`

Invalid examples:

- `api.buildkite`
- `foo.bar.buildkite`
- `*buildkite`
- `foo.*.buildkite`
- `*.buildkite.`
- `*.API.buildkite`

Each non-wildcard label should continue to obey the current DNS-label rules:

- lowercase letters, digits, and `-` only
- no empty labels
- no label longer than 63 bytes
- no label beginning or ending with `-`

Registration then builds the host pattern as:

- exact: `<name>.cleanroom.localhost`
- wildcard: `*.<suffix>.cleanroom.localhost`

### Route Matching

Route matching should be:

1. Normalize the request host.
2. Attempt an exact host lookup first.
3. If that fails, derive a single-label wildcard key by replacing the first
   label with `*` and look that up.
4. Return `404` if neither lookup matches.

This keeps wildcard behavior narrow and predictable. A wildcard route matches
exactly one leading label, not the base host and not arbitrarily deep names.

### Reverse Proxy Construction

Replace `httputil.NewSingleHostReverseProxy` with an explicit
`httputil.ReverseProxy{Rewrite: ...}` configuration.

Use a rewrite function so Cleanroom can safely set forwarding headers after the
standard library strips any client-supplied forwarding metadata.

The rewrite function should:

1. Set the internal target URL used for dialing the sandbox guest port.
2. Preserve `Out.Host = In.Host` so guest reverse proxies still route on the
   original host.
3. Call `ProxyRequest.SetXForwarded()` to set trusted forwarded host, proto,
   and client IP.
4. Add `X-Forwarded-Port` explicitly from the inbound request host or scheme
   default.

## Phased Implementation Plan

### Phase 1: Wildcard Route Syntax

1. Replace the current single-label exposure-name validator with a validator
   that accepts either an exact single label or a leading wildcard plus valid
   dotted suffix.
2. Reject explicit dotted exact names such as `api.buildkite`.
3. Add parser tests for valid wildcard shapes and invalid explicit dotted
   shapes.

### Phase 2: Wildcard Route Matching

1. Keep HTTPS routes keyed by their registered host or wildcard pattern.
2. Update `handleHTTPS` to do exact-match lookup first, then a derived
   single-label wildcard lookup.
3. Add route-matching tests covering exact matches, wildcard matches, and
   wildcard non-matches for base hosts and deeper hosts.

### Phase 3: Forwarded Headers

1. Replace the current `NewSingleHostReverseProxy` usage with a rewrite-based
   reverse proxy.
2. Preserve the original inbound `Host` header when proxying to the sandbox
   service.
3. Set trusted forwarded headers for upstream applications:
   - `X-Forwarded-Host`
   - `X-Forwarded-Proto`
   - `X-Forwarded-For`
   - `X-Forwarded-Port`
4. Add a helper to derive the forwarded port from `req.Host`, falling back to
   `443` or `80` when the host omits a port.
5. Add regression tests for forwarded-header propagation and redirect-safe
   upstream request context.

### Phase 4: Static Extra Certificate Domains

Current behavior:

- HTTPS route registration affects routing only.
- The HTTPS listener loads or generates one shared local certificate for
  `cleanroom.localhost`.
- That certificate is reused across all sandboxes and HTTPS exposures on the
  local machine.
- On macOS, `cleanroom dns install` also loads or generates that shared local
  certificate and installs trust for it.
- The current SAN set is fixed to:
  - `cleanroom.localhost`
  - `*.cleanroom.localhost`
- As a result, exact single-label hosts such as
  `buildkite.cleanroom.localhost` work today, but nested hosts such as
  `api.buildkite.cleanroom.localhost` fail TLS hostname validation even when
  wildcard routing exists.
- As currently implemented, the `cleanroom dns install` path still requests
  only the baseline SAN set, so it does not yet honor configured extra
  certificate domains and can replace an expanded certificate with the baseline
  one.

New behavior:

- HTTPS route registration remains the source of routing behavior.
- The certificate remains static while the HTTPS listener is running.
- Cleanroom loads a static configured set of additional certificate domains
  alongside the default SANs.
- These configured domains are used to build the shared exposure certificate at
  startup time and do not require runtime certificate regeneration.
- `cleanroom dns install` and HTTPS exposure startup resolve the same effective
  SAN set and use the same certificate generation and reuse rules.
- On macOS, rerunning `cleanroom dns install` after changing configured extra
  certificate domains refreshes trust for the regenerated shared certificate.

1. Keep the existing baseline SANs:
   - `cleanroom.localhost`
   - `*.cleanroom.localhost`
2. Add support for a static list of additional exposure certificate domains in
   Cleanroom server configuration.
3. Add support for the same list in project-level `cleanroom.yaml`, so a
   repository can declare the extra nested wildcard domains it needs.
4. Merge the project-level and server-level domain lists into one deduplicated
   effective SAN set, in addition to the default SANs.
5. Use the effective SAN set when loading or generating the shared local
   exposure certificate.
6. Treat these configured values as explicit certificate intent, not as derived
   runtime state from active exposures.
7. Do not reconfigure or regenerate the certificate when routes are added or
   removed while the exposure server is already running.
8. Require a fresh exposure-server start to pick up changes to the configured
   extra domain list.
9. Validate configured extra domains using the same wildcard shape constraints
   as route names where applicable:
   - allow exact domains when they are valid certificate SANs under
     `cleanroom.localhost`
   - allow leading wildcard domains such as `*.buildkite.cleanroom.localhost`
   - reject malformed or unsupported wildcard shapes
10. Keep the implementation backend-agnostic: configuration only changes the
    SAN set used by the shared local exposure certificate on the client.
11. Update `cleanroom dns install` to resolve the same effective SAN set from
    runtime config and project-level `cleanroom.yaml` as the HTTPS exposure
    startup path.
12. Use the same certificate load-or-generate path from `cleanroom dns
    install`, so DNS trust installation cannot replace a configured expanded
    certificate with a baseline-only certificate.
13. Add regression coverage proving direct TLS validation succeeds for nested
    hosts covered by configured wildcard SANs such as
    `api.buildkite.cleanroom.localhost`.
14. Add regression coverage proving `cleanroom dns install` and HTTPS exposure
    startup reuse the same certificate material for the same effective SAN set.

### Phase 5: Docs And End-To-End Verification

1. Update `docs/plans/sandbox-port-exposure.md` and the README examples to show
   wildcard registrations instead of explicit dotted aliases, and document the
   `exposure.certificate_domains` plus `cleanroom dns install` workflow needed
   for nested HTTPS trust.
2. Build a local Cleanroom binary from the branch under test.
3. Run `sudo cleanroom dns install` after configuring any extra exposure
   certificate domains needed for nested hosts.
4. Verify the wildcard exposure behavior end to end against
   `buildkite/buildkite` running in Cleanroom mode.

## Test Plan

- Add parser tests for:
  - `buildkite:3000`
  - `*.buildkite:3000`
  - `*.foo.bar.buildkite:3000`
  - rejected explicit dotted names such as `api.buildkite:3000`
  - invalid wildcard shapes
- Add configuration tests for:
  - project-level extra certificate domains
  - server-level extra certificate domains
  - merged and deduplicated effective SANs
- Add `dns install` tests proving it resolves the same effective certificate
  domains as HTTPS exposure startup and does not replace an expanded
  certificate with the baseline SAN set.
- Add exposure-manager tests that issue requests for:
  - `buildkite.cleanroom.localhost`
  - `api.buildkite.cleanroom.localhost`
  - `agent.buildkite.cleanroom.localhost`
  - `foo.bar.buildkite.cleanroom.localhost`
- Assert that:
  - `buildkite` matches only the exact route
  - `api.buildkite` and `agent.buildkite` match `*.buildkite`
  - `foo.bar.buildkite` does not match `*.buildkite`
- Add a backend test server that records the request seen by the guest proxy and
  assert:
  - `Host` is preserved
  - `X-Forwarded-Host` includes the external host and port
  - `X-Forwarded-Proto` is `https`
  - `X-Forwarded-Port` matches the HTTPS listener port
- Add TLS tests proving that configured extra wildcard SANs allow direct HTTPS
  validation for nested hosts such as `api.buildkite.cleanroom.localhost`.

## buildkite/buildkite Verification Criteria

- Configure `exposure.certificate_domains` to include
  `*.buildkite.cleanroom.localhost`.
- Run `sudo cleanroom dns install` after configuring the extra certificate
  domain so the trusted local certificate matches the effective SAN set.
- Start `buildkite/buildkite` in Cleanroom mode using one exact exposure and
  one wildcard exposure:

```bash
cleanroom exec \
  --expose-https buildkite:3000 \
  --expose-https '*.buildkite:3000' \
  -- bin/start
```

- Confirm `https://buildkite.cleanroom.localhost:<port>/` routes to the
  Buildkite web app, and unauthenticated redirects preserve the external
  `https` scheme and `<port>` in `Location` headers.
- Confirm `https://api.buildkite.cleanroom.localhost:<port>/` reaches the
  Buildkite app through the wildcard route without requiring an explicit
  `api.buildkite` registration. The response may be an app-level `401`, `403`,
  or Rails/API response, but it must not be a Cleanroom `404`.
- Confirm `https://agent.buildkite.cleanroom.localhost:<port>/v3/ping` reaches
  the Buildkite agent host through the wildcard route without requiring an
  explicit `agent.buildkite` registration. The response may fail
  authentication, but it must be a Buildkite response rather than a Cleanroom
  `404`.
- Confirm `https://foo.bar.buildkite.cleanroom.localhost:<port>/` returns a
  Cleanroom `404 page not found`, proving `*.buildkite` matches exactly one
  leading label.
- Confirm no external redirect generated by `buildkite/buildkite` falls back to
  `http`, drops the exposure port, or rewrites to a different host when reached
  through the wildcard exposure.
- Confirm rerunning `sudo cleanroom dns install` after the extra certificate
  domain is configured does not replace the trusted certificate with a
  baseline-only SAN set, for example by verifying nested-host TLS validation
  still succeeds afterward.

## Definition Of Done

- Wildcard HTTPS exposure names such as `*.buildkite` are accepted.
- Explicit dotted names such as `api.buildkite` are rejected.
- Exact and wildcard routes match as documented.
- Unmatched base or deeper hosts still return `404 page not found`.
- Upstream apps receive correct forwarded host, proto, and port headers.
- On macOS, `cleanroom dns install` uses the same effective SAN set as HTTPS
  exposure startup and does not overwrite configured nested-host certificates
  with a baseline-only SAN set.
- Docs describe wildcard-only nested-host registration clearly.
