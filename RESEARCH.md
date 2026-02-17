# Cleanroom research notes

## Objective

Understand candidate sandbox approaches for Cleanroom so we can decide what to implement directly, what to adapt, and what to keep as integration points.

Primary goal for v1:

- repository-scoped egress policy (deny-by-default)
- package manager and git fetch control through `content-cache`
- pluggable execution backends (local + remote)
- optionally first-class toolchain support for local agentic workflows (`mise`)
- secure secret injection

## Sources reviewed

- [earendil-works/gondolin](https://github.com/earendil-works/gondolin)
- [jingkaihe/matchlock](https://github.com/jingkaihe/matchlock) (validated at commit `ccce106411eca50c6f4b38ff9b83ac4416ef692e` on 2026-02-17)
- [wolfeidau/content-cache](https://github.com/wolfeidau/content-cache)
- [superfly/tokenizer](https://github.com/superfly/tokenizer)
- [Sprites Overview](https://docs.sprites.dev/)
- [Sprites Lifecycle and Persistence](https://docs.sprites.dev/concepts/lifecycle-and-persistence)
- [Sprites Networking](https://docs.sprites.dev/concepts/networking)
- [Sprites Checkpoints](https://docs.sprites.dev/concepts/checkpoints)
- [Sprites Services](https://docs.sprites.dev/concepts/services)
- [Sprites API reference (`v0.0.1-rc30`)](https://docs.sprites.dev/api/v001-rc30)
- [Sprites API: Policy](https://docs.sprites.dev/api/v001-rc30/policy)
- [Sprites API: Type definitions](https://docs.sprites.dev/api/v001-rc30/types)
- [Sprites release notes](https://sprites.dev/release-notes)
- [superfly/sprites-docs](https://github.com/superfly/sprites-docs)
- [superfly/sprites-go](https://github.com/superfly/sprites-go)
- [Cleanroom concept gist](https://gist.github.com/lox/cd5a74bee0c98e15c254e780bb73dd11)

## Shortlist reviewed

### 1) `gondolin`

Repo structure and observed architecture:

- Host orchestration is TypeScript/Node.
- Uses QEMU as a local VM layer.
- Multiple moving pieces:
  - control components on host
  - guest VM image and daemons (for example init-like services and filesystem mediation)
  - network control implemented in host + custom proxy/stack mediation

Key implementation characteristics:

- Strong containment model with custom VM filesystem and virtio control paths.
- Not a single binary product in current architecture.
- Policy and runtime flow is split across host packages, guest image assets, and service binaries.
- Good source of ideas for:
  - host-mediated network enforcement
  - VM boundary design
  - guest service bootstrap patterns
- Less aligned with v1 “single CLI binary” goal.

### 2) `matchlock`

Repo structure and observed architecture:

- Go-first implementation with clear backend abstraction.
- `pkg/vm` defines a backend interface.
- Linux path uses Firecracker; Darwin path uses Apple Virtualization Framework.
- `cmd/matchlock` / guest init path exists for command dispatch and lifecycle.

Key implementation characteristics:

- Backends are pluggable through a Go interface.
- Good executable-first ergonomics; likely easiest to integrate as a local execution backend.
- Includes network setup/entry points useful for deny-by-default style enforcement in v1.
- Policy-style engine exists for allowlisting and restrictions in-progress.
- Clean fit point for Cleanroom v1 local backend, especially for CLI-first toolchain.

### 3) `content-cache`

Repo structure and observed architecture:

- Dedicated package/cache proxy layer with routing support for package registries and git.
- Acts as policy-bearing mediation layer between builds and upstream.

Key implementation characteristics:

- Helps solve network minimization for package managers and git clone flows.
- Useful for:
  - allowed host filtering
  - route-based allow/deny behavior
  - cached fetches and repeatability
- Natural anchor for cleanroom’s registry and git controls.

### 4) `tokenizer`

Repo structure and observed architecture:

- Proxy-based secret injection model.
- Requests pass through a secret broker that applies destination policy and injects auth headers.

Key implementation characteristics:

- Useful pattern for short-lived, scoped secret use without embedding secrets in repo policy or commandlines.
- Fits Cleanroom requirement to keep secret material out of task environments and logs.

### 5) `sprites.dev`

Observed platform shape:

- Validation snapshot date: 2026-02-16 (including release notes through 2026-02-13).
- Sprites are positioned as persistent, hardware-isolated Linux execution environments (microVM-backed) with API/CLI/SDK control surfaces.
- Remote execution is a first-class model (create Sprite, execute commands, proxy ports, manage lifecycle/checkpoints/services).

Guarantees explicitly documented:

1. Isolation and environment model:
   - Hardware-level isolation via dedicated microVM is a core product claim.
   - Full Linux environment with persistent ext4 filesystem semantics.
   - Fixed resource profile (currently 8 vCPU, 8 GB RAM, 100 GB storage), not user-resizable yet.
2. Lifecycle and persistence:
   - Automatic hibernation after 30s inactivity (not configurable yet).
   - Wake-on-request behavior (CLI/API/HTTP) with resumed disk state.
   - Filesystem persists across hibernation; running processes, in-memory state, and live network connections do not.
3. Network and access control:
   - Sprite URL is private by default and token-authenticated.
   - URL auth can be switched to `public`.
   - Inbound access is via Sprite URL and/or explicit local port forwarding.
   - Outbound network access is full by default.
   - Network policy API supports domain rules with `allow`/`deny` actions plus optional policy include presets.
   - Policy changes apply immediately, and existing connections to newly blocked domains are terminated.
   - Blocked DNS lookups return `REFUSED`.
4. Checkpoint/restore:
   - Checkpoints capture persistent state (filesystem and config) and exclude process/memory/socket state.
   - Checkpoint create/restore is streamable over API/SDK and designed for frequent use.
   - Restores are destructive to current runtime state and require process restart.
   - Storage model is copy-on-write over object storage, with durability/geo-redundancy claims documented.
5. Long-lived process management:
   - Services are managed processes that auto-start on Sprite boot and survive full Sprite restarts.
   - Detached sessions are suitable for long-running tasks across disconnects.

Implications for Cleanroom v1:

- A deny-by-default remote egress posture is implementable using Sprites network policy (e.g. wildcard deny plus explicit allowlist).
- Policy updates are server-enforced and immediate, which is strong for post-start containment adjustments.
- Remote backend integration can remain thin: adapter translates Cleanroom policy model to Sprites policy payloads and validates post-apply behavior.
- Checkpoint support provides a useful primitive for task rollback, template baselining, and recovery.

Limits and caveats to treat as non-guarantees:

- No published SLA/uptime or formal durability SLO numbers were found in current public docs.
- Egress controls are documented as domain/DNS-policy behavior; protocol-level and IP-literal enforcement guarantees are not explicitly stated.
- API docs are versioned as release candidates (`v0.0.1-rc30`) with separate `dev` docs, so contract drift is a practical risk.
- Advanced policy surfaces like privileges appear in dev docs; they are not part of the stable RC API surface yet.
- Recent release notes confirm behavior is still moving (for example, wake/request handling and URL routing performance updates in February 2026).

Inference (explicitly inferred, not documented as hard guarantee):

- Because enforcement language is DNS/policy-rule based, Cleanroom should assume hostname-level enforcement unless direct-IP/DoH behavior is proven by integration tests.

## Comparison against Cleanroom requirements

| Requirement | Best fit |
|---|---|
| Enforce repository policy for egress | Local: `matchlock` + `content-cache`; Remote: Sprites policy API + `content-cache` host routing |
| Pluggable backends (local + remote) | `matchlock` backend abstraction + future remote adapter model |
| Package restrictions & caching | `content-cache` |
| Safe secret handling | `tokenizer` pattern (or equivalent in cleanroom control plane) |
| Git controls + registry controls | `content-cache` + Cleanroom-specific allowlist + lockfile logic |
| Single-binary distribution | `matchlock` adapter pattern is closest; `gondolin` is not naturally single binary |
| First-class local developer/agentic workflows (`mise`) | Better served by Cleanroom wrapping execution command path |

## What maps directly into v1 Cleanroom

1. Use `matchlock` as the local sandbox backend model.
2. Use `content-cache` as the package/registry and git mediation layer.
3. Use a tokenizer-like secret-injection model with host-scoped policy and no plaintext propagation.
4. Keep remote backend as adapter shell (sprites) with explicit policy payload transport.
5. Keep CLI first: `cleanroom exec` as primary entrypoint and command pattern.

## Firecracker backend proposal (initial implementation)

Build our own backend in Cleanroom, but reuse Matchlock techniques that are already proven in practice.

### Scope for initial backend

- Linux-only local backend using Firecracker + KVM.
- Enforce `CompiledPolicy` only (no runtime repo policy reload).
- Deny-by-default egress with explicit host/port allowlist.
- Route package and git egress through `content-cache`.
- Keep secret values out of guest env and policy files.

### Matchlock techniques to adopt directly

1. **Backend boundary and machine API**
   - Mirror the Matchlock split between backend creation and machine lifecycle (`Create/Start/Exec/Stop/Close`), but mapped to Cleanroom `BackendAdapter`.
   - Keep Firecracker-specific logic in a backend package, not in CLI or policy code.
2. **Per-run TAP + subnet allocation**
   - Create deterministic TAP names (hash/suffix by run ID), allocate a unique subnet per run, and configure TAP before VM start.
   - Reconcile subnet + TAP artifacts in cleanup and in a `gc` command.
3. **Generated Firecracker JSON config**
   - Generate boot source, machine config, drives, NIC, and vsock JSON on each run.
   - Keep kernel args host-controlled and deterministic.
4. **Vsock control plane**
   - Use vsock for guest readiness and command execution RPC.
   - Keep command execution out of SSH and avoid opening inbound guest ports.
5. **Rootfs preparation pattern**
   - Use per-run rootfs copy (prefer CoW/reflink where available, fallback to full copy).
   - Inject a small guest runtime entrypoint (`/init` + guest agent) before boot.
6. **Network interception strategy**
   - Use host nftables for Linux interception and forwarding, with explicit rule setup/teardown.
   - Keep a host-side policy/enforcement process for HTTP(S) interception and eventing.
7. **Lifecycle persistence + reconciliation**
   - Persist lifecycle phase and resource handles so crashed runs can be cleaned deterministically.
   - Make cleanup failures visible and auditable, not best-effort-only.

### Cleanroom-specific changes required (do not copy as-is)

1. **Policy semantics**
   - Matchlock allows all hosts when no allowlist is set; Cleanroom must default to deny.
   - Matchlock currently has allowlist-focused policy + private IP blocking; Cleanroom needs deterministic allow/deny precedence and normalized host matching from `SPEC.md`.
2. **Immutability and capability handshake**
   - Matchlock runtime config is mutable in places (for example auto-adding secret hosts). Cleanroom should only use compiled immutable policy and fail launch on missing capabilities.
3. **Registry mediation contract**
   - Cleanroom must force package manager and git paths through `content-cache` with lockfile artifact constraints, not generic outbound allowlist only.
4. **Reason-code and audit contract**
   - Cleanroom events must emit spec-defined stable codes (`host_not_allowed`, `lockfile_violation`, etc.) across CLI/API/audit logs.
5. **Secret model**
   - Cleanroom policy should carry secret IDs/bindings only; secret values resolved at runtime in control plane and never stored in repository config.

### Proposed Cleanroom architecture (v1 local Firecracker)

```text
cleanroom exec
  -> policy loader + compiler -> CompiledPolicy (immutable, hashed)
  -> capability validator (backend + policy requirements)
  -> firecracker adapter provision()
      -> state dir + lifecycle row
      -> rootfs copy/inject guest runtime
      -> start egress services:
           - content-cache (registry/git mediation)
           - optional secret proxy (tokenizer-style)
      -> TAP/subnet + nftables setup
      -> start firecracker + wait for vsock ready
  -> run command via vsock exec service
  -> stream logs + structured deny/allow events
  -> shutdown + deterministic cleanup/reconcile
```

### Capability mapping to `SPEC.md` requirements

| Capability key | Firecracker backend implementation (initial) |
|---|---|
| `network_default_deny` | nftables default drop for guest egress, explicit allow chains from compiled policy |
| `network_host_port_filtering` | host policy matcher + nftables/proxy enforcement for host and port outcomes |
| `dns_control_or_equivalent` | force guest DNS to managed resolvers and block unmanaged UDP DNS paths |
| `policy_immutability` | adapter accepts only serialized `CompiledPolicy` and stores policy hash in run state |
| `audit_event_emission` | host-side event bus emits stable reason codes with `run_id` and `policy_hash` |
| `secret_isolation` | secret values held in host proxy/control process only; guest sees placeholders or no secret values |

### Incremental implementation plan

1. **Slice A: minimal Firecracker runner**
   - Create backend adapter package and run lifecycle.
   - Boot VM, run command over vsock, collect exit code/stdout/stderr.
2. **Slice B: deterministic networking**
   - Add TAP/subnet allocator + nftables setup/teardown.
   - Enforce default deny and exact host/port allowlist (no registries yet).
3. **Slice C: registry and git mediation**
   - Start/attach `content-cache`.
   - Rewrite package/git traffic through cache endpoint and emit deny reasons for bypass attempts.
4. **Slice D: secret proxy**
   - Add tokenizer-style host-scoped injection path.
   - Enforce `secret_scope_violation` and keep secret values out of guest-visible env/args.
5. **Slice E: conformance and hardening**
   - Implement backend capability handshake.
   - Add conformance suite from `SPEC.md` section 14 before backend marked supported.

### Immediate engineering decisions for implementation kickoff

1. Use Matchlock-style vsock exec and readiness signaling rather than SSH.
2. Use nftables on Linux as the first enforcement mechanism (simple, auditable, and already aligned with Matchlock patterns).
3. Keep rootfs mutation host-side with a tiny guest runtime, but keep it minimal to reduce boot drift.
4. Build lifecycle DB + reconciliation early, before adding advanced policy features, to avoid leaked TAP/nftables resources.
5. Wire `content-cache` before implementing lockfile strictness so path mediation is in place first.

## Key design inferences from the research

- `gondolin` has strong isolation ideas but is a larger multi-component platform. It is best treated as design inspiration for network and filesystem controls rather than a direct v1 embed.
- `matchlock` is the pragmatic base for local execution because it already provides:
  - backend abstraction
  - VM strategy in-process
  - command surface suitable for a wrapper binary
- `sprites.dev` provides a practical remote execution control plane now, with meaningful policy/lifecycle primitives, but its policy model should be treated as domain-oriented until deeper protocol-level enforcement is validated.
- `content-cache` and tokenizer-like secret mediation are cleaner to integrate as policy-aware external services than to reinvent in phase 1.

## Risks and unknowns still to validate

- Matchlock network enforcement edge cases (DNS interception and port-level blocking exactness).
- Sprites remote policy enforcement beyond DNS/domain matching (direct-IP and DoH behavior).
- Sprites API version drift risk between RC and dev surfaces for policy/runtime features.
- Missing public SLA/SLO guarantees for availability/durability.
- Lockfile parser maturity by ecosystem.
- `content-cache` deployment topology for local-only vs remote-only workflows.

## Next concrete work after research

- Define `internal/backend/firecracker` package boundaries:
  - machine lifecycle
  - network setup (tap/subnet/nftables)
  - guest exec transport (vsock)
  - lifecycle persistence/reconcile
- Add capability handshake and launch validation path from compiled policy.
- Implement a first conformance subset:
  - default deny
  - explicit allow host/port
  - explicit deny precedence
  - stable reason codes in events
- Integrate `content-cache` in the backend provisioning flow before lockfile strict mode.
- Add implementation tasks for:
  - cleanroom-first command model (`exec`/`run` alignment)
  - `mise` detection and bootstrap strategy
  - lockfile-restricted package allowlists
  - secret injection guardrails
