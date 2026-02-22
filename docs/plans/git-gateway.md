# Git Gateway MVP Plan

## Summary

This plan defines an MVP for a controllable Git endpoint in Cleanroom that enables
`git clone`/`fetch` inside the sandbox without changing user commands.

MVP goal:
- Route Git smart-HTTP traffic through a Cleanroom-managed gateway.
- Enforce allowlisted Git destinations from policy.
- Support quick repo sandboxing with optional local mirror acceleration.

Review-driven adjustments included in this revision:
- Use a vsock transport between guest and host (not guest->host loopback assumptions).
- Scope URL rewrites by allowed host(s), not a global `https://` catch-all.
- Sequence phases so policy integration lands before allowlist enforcement acceptance checks.

Out of scope for MVP:
- Push (`git-receive-pack`) and PR write flows.
- Full secret injection/tokenizer integration.
- Workspace upload/mount for uncommitted host trees.

## Non-Goals (MVP)

- Replacing existing package content-cache behavior.
- Designing a full multi-tenant Git hosting product.
- Protocol translation for SSH remotes.

## Desired User Experience

Users should be able to run existing commands in a sandbox unchanged:

```bash
git clone https://github.com/org/repo.git
git fetch
```

without manually setting proxy flags, while Cleanroom enforces policy and emits
auditable allow/deny events.

## Proposed Architecture

1. Host-side Git gateway process
- Started and managed by Cleanroom runtime during sandbox provisioning.
- Listens on a host vsock port (for example `CID=host, port=17080`) via a narrow
  HTTP-over-vsock transport adapter.
- Handles smart-HTTP fetch endpoints:
  - `GET /git/<host>/<org>/<repo>.git/info/refs?service=git-upload-pack`
  - `POST /git/<host>/<org>/<repo>.git/git-upload-pack`

2. Guest-local relay shim
- A tiny in-guest relay listens on `127.0.0.1:<port>` and forwards HTTP requests
  over vsock to the host-side gateway.
- This preserves normal Git HTTP client behavior while keeping host services off
  routed guest network paths.

3. Sandbox Git URL rewrite (scoped)
- Runtime injects temporary Git config entries per allowed host, for example:
  - `url."http://127.0.0.1:<port>/git/github.com/".insteadOf=https://github.com/`
- Commands continue to use upstream `https://` remotes; Git rewrites internally.

4. Policy-driven enforcement in gateway
- Parse target host/repo from rewritten path.
- Allow only policy-approved destinations; deny otherwise.
- Emit stable reason codes for deny decisions.

5. Source mode
- `upstream` mode: proxy to upstream origin over HTTPS.
- `host_mirror` mode: prefer local bare mirror (if present), else fallback to upstream.

## Policy Shape (MVP)

Add minimal policy keys under `sandbox.git`:

```yaml
sandbox:
  git:
    enabled: true
    source: upstream # upstream | host_mirror
    allowed_hosts:
      - github.com
    allowed_repos:
      - org/repo
      - org/another-repo
```

Notes:
- `allowed_repos` is optional; if omitted, host-level allow applies.
- Matching is exact in MVP (`host`, `org/repo`) to avoid wildcard ambiguity.

## API/Runtime Changes

No external control-plane RPC changes required for MVP.

Implementation touches:
- Policy compiler: parse/validate `sandbox.git` block into compiled policy.
- Runtime backend provisioning: start/stop gateway lifecycle with sandbox.
- Guest execution environment: start/stop relay shim and inject temporary scoped Git config.
- Observability/eventing: add Git allow/deny records.

## Phased Implementation

## Phase 1: Policy + Transport Foundations

Scope:
- Policy parser/compiler support for `sandbox.git`.
- Implement guest relay + host gateway transport over vsock.
- Upstream proxy mode only.

Deliverables:
- Validation errors for invalid/empty host/repo entries.
- Internal packages for relay, gateway server, and route parsing.
- Scoped rewrite injection helper (per-host `insteadOf` entries).

Acceptance:
- A sandbox can reach gateway only through the guest relay over vsock.
- Scoped rewrite entries are generated only for configured `allowed_hosts`.

## Phase 2: Gateway Read Path Enforcement

Scope:
- Implement smart-HTTP fetch endpoints in gateway.
- Enforce allowlist decisions from compiled policy.

Deliverables:
- Request handling for `info/refs?service=git-upload-pack` and `git-upload-pack`.
- Deny-path reason mapping and structured event emission.

Acceptance:
- `git clone https://github.com/<allowed>/repo.git` succeeds in sandbox.
- Non-allowlisted host/repo returns explicit deny reason.
- Existing commands require no user rewrite/proxy flags.

## Phase 3: Host Mirror Fast Path

Scope:
- Add `source: host_mirror` behavior.
- Resolve local bare mirror path and serve/proxy accordingly.

Deliverables:
- Mirror resolver logic with fallback to upstream.
- Tests for mirror hit/miss fallback behavior.

Acceptance:
- Mirror-backed clone works when mirror exists.
- Missing mirror falls back without breaking clone.

## Security Controls (MVP)

- Deny by default for all Git destinations not in policy.
- No plaintext credentials stored in repository policy.
- Host gateway is not exposed on guest-routable TCP interfaces.
- Gateway strips/controls headers forwarded upstream.
- Structured deny reasons include at least:
  - `host_not_allowed`
  - `repository_not_allowed`
  - `runtime_launch_failed` (gateway startup failure path)

## Observability

Emit per-request structured events with:
- `run_id`
- `sandbox_id`
- `backend`
- `request_host`
- `request_repo`
- `decision` (`allow`/`deny`)
- `reason_code`
- latency and upstream/cache source indicator

## Testing Strategy

1. Unit tests
- Policy parsing/validation for `sandbox.git`.
- Path/host/repo parsing and match logic.
- Deny reason mapping.

2. Integration tests
- Sandbox clone success for allowlisted repo.
- Deny for disallowed host/repo.
- Guest relay over vsock works; direct host loopback access is not required.
- `host_mirror` resolution and fallback behavior.

3. Smoke tests
- `cleanroom exec -- git clone https://...`
- `cleanroom exec -- git fetch`

## Risks and Mitigations

- Risk: Smart HTTP edge-case incompatibilities across Git versions.
  - Mitigation: keep gateway as transparent as possible; add protocol-level fixture tests.

- Risk: Over-broad rewrite captures unrelated HTTPS usage.
  - Mitigation: per-host `insteadOf` entries only; no global `https://` rewrite.

- Risk: vsock transport could regress under very high concurrency.
  - Mitigation: keep relay/gateway streaming and benchmark clone/fetch throughput against TAP path.

- Risk: Local mirror path trust boundary confusion.
  - Mitigation: explicit `host_mirror` mode gate and strict path validation.

## Follow-Up Work (Post-MVP)

- Add push support (`git-receive-pack`) with authenticated write policy.
- Integrate short-lived upstream auth token injection.
- Add workspace ingress flow (upload/mount/snapshot) for uncommitted local trees.
- Expand policy matching semantics (wildcards, org-level rules) with deterministic precedence.
