# Host Gateway Implementation Plan

**Spec reference:** SPEC.md §6.2
**Status:** Proposed

## Summary

A single shared host-side HTTP gateway that mediates sandbox access to external services (git, package registries, secrets, metadata). Sandbox identity is derived from TAP network source IPs — no bearer tokens, no per-sandbox listeners.

## Background

### Why not vsock?

The guest exec agent uses vsock in the guest-listen/host-dial direction, which works because Firecracker mediates it through its own UDS path. The gateway requires the reverse — guest dials host — which needs a host-side `AF_VSOCK` listener.

In nested virtualisation (Firecracker inside EC2), host-side vsock binds on `CID=2` (host) and `CID=0` (any) fail with `cannot assign requested address`. The outer hypervisor owns the vsock device and CID namespace, and the `vhost_vsock` driver cannot serve both roles simultaneously.

TAP-network TCP has no such constraint — it is managed entirely within the host kernel's networking stack.

### Why not per-sandbox listeners or scope tokens?

- **Per-sandbox listeners** create O(sandboxes × services) file descriptors and lifecycle management overhead. At 1000 sandboxes this becomes a resource and orchestration problem.
- **Scope tokens** (the prior `git-gateway-mvp-plan` branch approach) rely on bearer token secrecy for isolation. Token exfiltration via logs, error messages, or `ps` output could grant cross-sandbox access. Source-IP identity eliminates this class of vulnerability entirely.

### Why source-IP identity works

Each sandbox already gets a unique TAP device and guest IP (`10.x.y.2` derived from run ID). The host-side iptables rules prevent cross-TAP traffic. With anti-spoof rules added, the guest IP becomes a hard identity — even a root-privileged guest cannot spoof another sandbox's source IP because traffic is constrained to its own TAP interface.

## Architecture

```
sandbox A (10.1.1.2)                sandbox B (10.2.2.2)
       |                                   |
    [tap-A]                             [tap-B]
       |                                   |
       +------- host gateway :8170 --------+
                      |
            lookup source IP
           in sandbox registry
                      |
              enforce policy A or B
                      |
                route by path
               /      |      \
            /git/  /registry/  /secrets/
              |
         proxy upstream
       (inject host-side credentials)
```

The gateway listens on a single port. Every connection's source IP is mapped to a sandbox and its `CompiledPolicy`. Service routing is by path prefix.

## Existing code touchpoints

| File | Change |
|---|---|
| `internal/backend/firecracker/backend.go` | `setupHostNetwork`: add anti-spoof INPUT rules. `ProvisionSandbox`/`TerminateSandbox`: register/release sandbox in gateway. `executeInSandbox`: inject gateway env vars into exec requests. |
| `internal/backend/backend.go` | Add gateway registry interface to adapter or provision request. |
| `internal/controlservice/service.go` | Own the gateway lifecycle — start lazily, pass registry to backend adapter. |
| `internal/policy/policy.go` | No changes needed. `CompiledPolicy` and `AllowRule` already carry what the gateway needs. |
| `cmd/cleanroom-guest-agent/main.go` | No changes needed. Gateway env vars are injected per-exec via the vsockexec protocol. |

## Implementation slices

### Slice 1: Gateway skeleton and identity registry

**New package: `internal/gateway`**

**`registry.go`** — Thread-safe sandbox scope registry.

```go
type SandboxScope struct {
    SandboxID string
    Policy    *policy.CompiledPolicy
}

type Registry struct { ... }

func (r *Registry) Register(guestIP, sandboxID string, p *policy.CompiledPolicy)
func (r *Registry) Release(guestIP string)
func (r *Registry) Lookup(guestIP string) (*SandboxScope, bool)
```

Keyed by guest IP string. Populated during `ProvisionSandbox`, released during `TerminateSandbox`.

**`server.go`** — HTTP server with source-IP identity middleware.

- Listens on `0.0.0.0:8170` (configurable via runtime config).
- Middleware: extract source IP from `http.Request.RemoteAddr` (strip port, handle IPv6-mapped v4), call `Registry.Lookup()`, inject `*SandboxScope` into request context. Return 403 if not found.
- Path router mounts service handlers at `/git/`, `/registry/`, `/secrets/`, `/meta/`.
- Initial handlers: `/git/` wired in slice 3; others return 501.

**`path.go`** — Request path canonicalisation.

- Reject paths containing `..`, `//`, null bytes, or percent-encoded traversal sequences before routing.
- Normalise path before prefix matching.

**Wiring:**

- Control service creates `*gateway.Registry` and `*gateway.Server` at startup.
- Pass registry reference to the firecracker adapter.
- Adapter calls `Register` / `Release` during sandbox lifecycle.

**Tests:**

- Registry: concurrent register/release/lookup, duplicate IP handling, release of unknown IP.
- Middleware: source IP extraction from various `RemoteAddr` formats (`10.1.1.2:43210`, `[::ffff:10.1.1.2]:43210`), 403 for unregistered IPs.
- Path canonicalisation: traversal rejection, normalisation.

**Definition of done:** Gateway process starts, accepts HTTP connections, resolves sandbox identity from source IP, returns 403 for unknown callers and 501 for unimplemented service paths.

---

### Slice 2: Anti-spoof and INPUT rules

**Modify `setupHostNetwork` in `internal/backend/firecracker/backend.go`.**

Add after TAP creation, before existing FORWARD rules:

```
# Anti-spoof: drop anything from this TAP not sourced from the assigned guest IP
iptables -A INPUT -i tapX ! -s GUEST_IP -j DROP

# Allow guest to reach gateway port
iptables -A INPUT -i tapX -s GUEST_IP -p tcp --dport 8170 -j ACCEPT

# Drop all other host INPUT from this TAP
iptables -A INPUT -i tapX -j DROP
```

Rule ordering matters — the ACCEPT for the gateway port must come before the catch-all DROP. Add corresponding cleanup entries in reverse order.

**Additional hardening:**

- Disable IPv6 on TAP: `sysctl -w net.ipv6.conf.tapX.disable_ipv6=1`
- Verify gateway port is not reachable from non-TAP interfaces (document as operator responsibility for v1; consider a global INPUT rule in `cleanroom serve` startup).

**Tests:**

- Unit test rule generation order and cleanup commands.
- Integration test (on Linux with iptables): verify gateway port reachable from sandbox guest IP, unreachable from other source IPs.

**Definition of done:** Anti-spoof and gateway INPUT rules are installed per sandbox and cleaned up on termination. iptables rule ordering is tested.

---

### Slice 3: Git smart-HTTP proxy

**`internal/gateway/git.go`** — Handler for `/git/<upstream-host>/<owner>/<repo>[.git]/...`

**Request flow:**

1. Extract upstream host from first path segment after `/git/`.
2. Validate upstream host against sandbox's `CompiledPolicy.Allow` (must allow the host on port 443).
3. Proxy two git smart-HTTP endpoints:
   - `GET /git/<host>/<owner>/<repo>.git/info/refs?service=git-upload-pack`
   - `POST /git/<host>/<owner>/<repo>.git/git-upload-pack`
4. Deny `git-receive-pack` (push) — sandboxes are read-only.
5. Inject `Authorization` header from credential provider (slice 4) on upstream request.
6. Stream response back to guest.

**Upstream transport:**

- Per-sandbox `http.Transport` (or transport keyed by sandbox ID). No connection pool sharing across sandbox identities.
- Timeout on upstream requests (30s default, configurable).

**Audit events:**

- Emit structured log per request: `sandbox_id`, `upstream_host`, `repo_path`, `action` (allow/deny), `reason_code`.
- Deny reasons: `host_not_allowed` (upstream host not in policy), `method_not_allowed` (push attempt).

**Guest-side wiring (in `executeInSandbox`):**

Inject git URL rewrite config via environment variables in the exec request:

```
GIT_CONFIG_COUNT=N
GIT_CONFIG_KEY_0=url.http://10.x.y.1:8170/git/github.com/.insteadOf
GIT_CONFIG_VALUE_0=https://github.com/
```

One pair per allowed git host in the sandbox's policy. The host IP is the sandbox's own TAP host-side IP, so it is unique per sandbox and routed through the sandbox's own TAP.

**Tests:**

- Unit: path parsing (`/git/github.com/org/repo.git/info/refs`), host extraction, policy validation.
- Unit: `git-receive-pack` rejection.
- Unit: path traversal (`/git/../secrets/`) blocked by slice 1 canonicalisation.
- Integration: end-to-end `git clone` through the gateway against a test git server.

**Definition of done:** A sandbox can `git clone https://github.com/org/repo.git` and the request is transparently proxied through the gateway with policy enforcement and upstream credential injection.

---

### Slice 4: Credential provider

**`internal/gateway/credentials.go`** — Interface for resolving upstream credentials.

```go
type CredentialProvider interface {
    Resolve(ctx context.Context, upstreamHost string) (token string, err error)
}
```

**Initial implementation: `EnvCredentialProvider`**

- Reads from environment variables at gateway startup: `CLEANROOM_GITHUB_TOKEN`, `CLEANROOM_GITLAB_TOKEN`, etc.
- Maps upstream host to credential: `github.com` → `CLEANROOM_GITHUB_TOKEN`.
- Returns empty string (no auth) if no credential is configured for the host.

**Future: `GitHubAppCredentialProvider`**

- Mints short-lived GitHub App installation tokens per upstream request.
- Caches tokens by installation ID with TTL.
- Out of scope for initial implementation but the interface should accommodate it.

**Security invariants:**

- Credential values never appear in audit logs, error messages, or gateway responses.
- Credentials are resolved per-request, never stored in the sandbox registry.

**Tests:**

- Env provider: resolution by host, missing credential returns empty, env variable not leaked in errors.

**Definition of done:** Git proxy injects upstream `Authorization: Bearer <token>` headers for configured hosts.

---

### Slice 5: Registry proxy (stub)

**`internal/gateway/registry.go`** — Handler for `/registry/...`

Initial implementation is a passthrough HTTP proxy with policy enforcement:

1. Extract upstream registry host from request path.
2. Validate against sandbox's compiled policy registry allowlist.
3. Proxy request upstream, injecting credentials if configured.
4. Return `registry_not_allowed` on deny.

This is where content-cache integration can plug in later — the gateway either handles proxying directly or forwards to a content-cache instance for caching and lockfile enforcement.

**Tests:**

- Policy enforcement: allowed registry host proxied, disallowed host denied with correct reason code.

**Definition of done:** Package manager requests routed through `/registry/` are policy-enforced and proxied upstream.

---

### Slice 6: Conformance tests

Validate the security properties from SPEC.md §6.2:

| Test | Validates |
|---|---|
| Unregistered source IP returns 403 | Transport identity is required |
| Sandbox A cannot access sandbox B's policy scope | Cross-sandbox isolation |
| Git clone to policy-allowed host succeeds | Positive path works |
| Git clone to non-allowed host returns `host_not_allowed` | Policy enforcement |
| Git push (`receive-pack`) is rejected | Read-only enforcement |
| `/git/../../secrets/` returns 400 | Path traversal hardening |
| Gateway unreachable from non-TAP source | INPUT rules correct |
| Anti-spoof rules installed and cleaned up | Lifecycle correctness |
| Audit events contain sandbox ID and reason code | Attribution works |
| Upstream credentials not in audit logs or responses | Secret isolation |

**Definition of done:** All conformance tests pass on Linux with Firecracker backend.

## Sequencing and dependencies

```
Slice 1 (skeleton + registry)
    |
    +---> Slice 2 (anti-spoof + INPUT rules)
    |         |
    |         +---> Slice 6 (conformance tests)
    |
    +---> Slice 3 (git proxy)
              |
              +---> Slice 4 (credential provider)
              |
              +---> Slice 5 (registry proxy)
```

Slices 1 + 2 land together as the foundation. Slices 3 + 4 can proceed in parallel with slice 5. Slice 6 runs after slices 1–3 are complete.

| Slice | Effort | Notes |
|---|---|---|
| 1: Gateway skeleton + registry | S | New package, no external dependencies |
| 2: Anti-spoof + INPUT rules | S | Additive change to existing `setupHostNetwork` |
| 3: Git smart-HTTP proxy | M | Core feature, needs integration testing |
| 4: Credential provider | S | Interface + env-based implementation |
| 5: Registry proxy | M | Similar shape to git proxy |
| 6: Conformance tests | S | Mostly wiring existing primitives |

## Risks

- **IP collision**: `hostGuestIPs` derives IPs from SHA-1 of run ID, using only 2 bytes. Two concurrent sandboxes could collide on the same `10.x.y.0/24`. The registry would reject the second registration. Mitigation: detect collision at registration time and fail sandbox creation with a clear error. Longer term: allocate from a managed pool instead of hashing.
- **Gateway as bottleneck**: A single gateway process handles all sandbox traffic. At very high density, this could become a CPU or connection bottleneck. Mitigation: monitor request latency and concurrency; scale with `SO_REUSEPORT` workers if needed.
- **iptables rule accumulation**: At 1000 sandboxes, the INPUT and FORWARD chains become long. Mitigation: migrate to nftables with per-sandbox named chains or ipsets for O(1) lookup.
