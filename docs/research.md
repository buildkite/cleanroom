# Cleanroom research notes

## Objective

Understand candidate sandbox approaches for Cleanroom so we can decide what to implement directly, what to adapt, and what to keep as integration points.

## Document status

- Last reviewed: 2026-02-20.
- Decision status for local Linux backend: keep Firecracker as default for v1.
- Volatile claims (releases, public API surface, product guarantees) should include a validation snapshot date.

Primary goal for v1:

- repository-scoped egress policy (deny-by-default)
- package manager and git fetch control through `content-cache`
- pluggable execution backends (Firecracker on Linux for v1)
- optionally first-class toolchain support for local agentic workflows (`mise`)
- secure secret injection

## Sources reviewed

### Cleanroom and related baselines

- [Cleanroom concept gist](https://gist.github.com/lox/cd5a74bee0c98e15c254e780bb73dd11)
- [earendil-works/gondolin](https://github.com/earendil-works/gondolin)
- [jingkaihe/matchlock](https://github.com/jingkaihe/matchlock) (validated at commit `9db058b9cddaf2769a201b5e67010e8b10f8b76e` on 2026-02-20)

### Content and secret mediation

- [wolfeidau/content-cache](https://github.com/wolfeidau/content-cache)
- [superfly/tokenizer](https://github.com/superfly/tokenizer)

### Firecracker

- [firecracker-microvm/firecracker](https://github.com/firecracker-microvm/firecracker) (validated release `v1.14.1`, published 2026-01-20)
- [firecracker-microvm/firecracker releases](https://github.com/firecracker-microvm/firecracker/releases)
- [Firecracker FAQ](https://github.com/firecracker-microvm/firecracker/blob/main/FAQ.md)
- [Firecracker design](https://github.com/firecracker-microvm/firecracker/blob/main/docs/design.md)
- [Firecracker production host setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md)

### libkrun

- [containers/libkrun](https://github.com/containers/libkrun) (validated release `v1.17.4`, published 2026-02-18)
- [containers/libkrun releases](https://github.com/containers/libkrun/releases)

### Image distribution and VM image tooling

- [cirruslabs/tart](https://github.com/cirruslabs/tart) (validated at commit `8f8a24ad19f8335640db0d30f9619605612d34a7` on 2026-02-20)
- [Tart quick start](https://tart.run/quick-start/)
- [Tart FAQ](https://tart.run/faq/)
- [codecrafters-io/oci-image-executor](https://github.com/codecrafters-io/oci-image-executor) (validated at commit `abc343907821d5089764dbdf06b4e2738e838bf6` on 2026-02-20)
- [WoodProgrammer/firecracker-rootfs-builder](https://github.com/WoodProgrammer/firecracker-rootfs-builder) (validated at commit `6e539e01e79307c68ba92990668cc33433d41175` on 2026-02-20)

### Cross-platform enforcement references

- [eBPF Docs: eBPF on Linux](https://docs.ebpf.io/linux/)
- [Apple Developer: `NEDNSProxyProvider`](https://developer.apple.com/documentation/networkextension/nednsproxyprovider)
- [Apple Technical Note TN3165](https://developer.apple.com/documentation/technotes/tn3165-packet-filter-is-not-api)
- [Apple Support: DNS Proxy payload settings](https://support.apple.com/guide/deployment/dns-proxy-payload-settings-dep500f65271/web)
- [Objective-See LuLu](https://objective-see.org/products/lulu.html)
- [Cloudflare WARP modes](https://developers.cloudflare.com/cloudflare-one/team-and-resources/devices/warp/configure-warp/warp-modes/)

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
- Image flow is already close to what Cleanroom needs:
  - accepts OCI-style image refs for `run`, with optional forced pull
  - supports explicit image lifecycle commands (`pull`, `build`, `image ls/rm/import`)
  - converts OCI image contents into ext4 rootfs artifacts and caches them locally
  - stores image metadata (tag/ref, digest, size, OCI config, source) in SQLite with `local` and `registry` scopes
  - preserves Docker-like `ENTRYPOINT`/`CMD`/`WORKDIR`/`ENV` semantics by carrying OCI config metadata into runtime command composition

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

## Comparison against Cleanroom requirements

| Requirement | Best fit |
|---|---|
| Enforce repository policy for egress | Local: Firecracker + `content-cache` |
| Pluggable backends | Cleanroom adapter interface + Firecracker backend |
| Package restrictions & caching | `content-cache` |
| Safe secret handling | `tokenizer` pattern (or equivalent in cleanroom control plane) |
| Git controls + registry controls | `content-cache` + Cleanroom-specific allowlist + lockfile logic |
| Single-binary distribution | `matchlock` adapter pattern is closest; `gondolin` is not naturally single binary |
| First-class local developer/agentic workflows (`mise`) | Better served by Cleanroom wrapping execution command path |

## What maps directly into v1 Cleanroom

1. Use Firecracker as the local sandbox backend (inspired by Matchlock patterns).
2. Use `content-cache` as the package/registry and git mediation layer.
3. Use a tokenizer-like secret-injection model with host-scoped policy and no plaintext propagation.
4. Keep CLI first: `cleanroom exec` as primary entrypoint and command pattern.

## Base image management recommendation (Matchlock-style)

Validation snapshot date: 2026-02-20.

Recommendation:

- Use OCI images as the authoring/distribution interface, and materialize/cached ext4 rootfs files for Firecracker runtime.
- Adopt Matchlock-style image workflow shape (`pull/build/import/ls/rm`) with Cleanroom policy semantics and stricter immutability.
- Require digest-pinned image references in repository policy (`cleanroom.yaml`), not mutable tags, for reproducibility and auditability.

Proposed policy shape (backend-neutral):

```yaml
version: 1
sandbox:
  image:
    ref: ghcr.io/your-org/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

Behavioral contract for v1:

1. `sandbox.image.ref` must be a digest-pinned OCI reference (`@sha256:...`).
2. Launch fails if image ref is tag-only (for example `:latest`) or digest validation fails.
3. Image manager:
   - resolves and pulls OCI image by digest,
   - materializes ext4 rootfs once,
   - caches by digest locally,
   - records metadata (digest, source ref, size, OCI config) in an image metadata DB.
4. Firecracker adapter then performs per-run rootfs copy/CoW from cached base rootfs.
5. Run events include `image_ref` and `image_digest` for audit.

Why this is the right fit:

- It preserves a standard image distribution UX (OCI registries, digest pinning).
- It keeps Firecracker runtime requirements native (kernel + ext4 paths), avoiding runtime container stack complexity.
- It matches current Cleanroom architecture direction (immutable compiled policy + deterministic per-run isolation).

### Notes from adjacent tooling

- Tart confirms a strong OCI-distribution model for VM images, but it uses Tart-specific OCI artifact media types and a local VM cache layout; it is not a direct Firecracker runtime path.
- `oci-image-executor` is useful reference for OCI-image-to-ext4 execution flow, but it is tar-export based (`docker export`) and not a complete modern image lifecycle/caching model.
- `firecracker-rootfs-builder` is a useful minimal Dockerfile-to-rootfs helper concept, but it currently lacks mature registry/distribution semantics and should be treated as idea input rather than direct baseline.

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
6. **Digest-pinned image policy**
   - Matchlock permits tag-based flows (`alpine:latest`) for convenience; Cleanroom should require digest-pinned image refs in `cleanroom.yaml` for deterministic runs and stronger provenance.

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
6. Add repository policy field for base image reference and require digest-pinned OCI refs (`@sha256:...`) at launch time.

## `libkrun` vs Firecracker for Cleanroom local backend (deep dive)

Validation snapshot date: 2026-02-20.

Primary references for this section:

- [Firecracker README](https://github.com/firecracker-microvm/firecracker/blob/main/README.md)
- [Firecracker design](https://github.com/firecracker-microvm/firecracker/blob/main/docs/design.md)
- [Firecracker production host setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md)
- [libkrun README](https://github.com/containers/libkrun/blob/main/README.md)
- [libkrun releases](https://github.com/containers/libkrun/releases)

### Upstream status (documented)

- `libkrun`: latest release is `v1.17.4` (published 2026-02-18).
- Firecracker: latest release is `v1.14.1` (published 2026-01-20).

This is not a "stale project vs active project" decision; both are actively maintained.

### Security and network model (documented facts)

| Dimension | Firecracker | `libkrun` | Cleanroom implication |
|---|---|---|---|
| Positioning | Firecracker is purpose-built for secure multi-tenant workloads with a minimal device model and attack-surface reduction. | `libkrun` is a dynamic library for partially isolated execution with a simple C API. | Firecracker aligns better with strict untrusted-workload isolation goals for Linux CI/agent runs. |
| Process isolation guidance | Firecracker design and production docs call for defense in depth: seccomp, cgroups, namespaces, and jailer-based privilege dropping. | `libkrun` security model states guest and VMM should be treated as one security context and VMM isolation must be done with host OS controls (for example namespaces). | With `libkrun`, more hardening responsibility shifts to Cleanroom/operator policy around the VMM process itself. |
| Network behavior | Conventional microVM NIC model that naturally fits TAP + host firewall policy patterns. | Two mutually exclusive network modes: `virtio-vsock + TSI` (no virtual NIC) or `virtio-net + passt/gvproxy` (userspace proxy path). TSI docs state guest and VMM are effectively in the same network context. | Deterministic host/port allowlist enforcement and reason-code auditing are simpler to reason about in the Firecracker model for v1 Linux. |
| Integration surface | External VMM with stable machine config surface and mature production guidance. | Embeddable library with C API and network/proxy choices that depend on host process controls. | `libkrun` is attractive for embedding, but adoption would still require backend-specific security and networking redesign. |

### Fit against current Cleanroom backend shape (inference from repo implementation)

- Current Linux backend is explicitly Firecracker and Linux/KVM scoped.
- Current deny-by-default enforcement path is built around per-run TAP + host firewall FORWARD rules.
- Current launch flow shells out to a Firecracker binary with generated machine JSON and vsock guest-agent RPC.

Inference: swapping to `libkrun` would be a backend re-architecture, not a drop-in runtime substitution.

### Decision for v1 Linux backend

- Keep Firecracker as the default local Linux backend.
- Re-evaluate `libkrun` as an optional secondary backend where capability downgrades are explicit (for example, local macOS workflows with different enforcement guarantees).

## Cross-platform filtering findings (`Linux` + macOS)

Validation snapshot date: 2026-02-17.

1. There is no single kernel-level filtering substrate we can share across Linux and macOS.
   - Linux path can use eBPF and nftables.
   - macOS path should use Network Extension providers (for example DNS proxy provider / network filter provider model), not Linux packet-filter assumptions.
2. DNS resolver-level filtering is practical on both platforms, but with different enforcement primitives.
   - Linux: resolver control + egress firewall policy.
   - macOS: DNS proxy payload / DNS proxy extension model.
3. Prior art for reliable macOS filtering exists and is active.
   - LuLu documents a Network Extension-based firewall model and calls out operational constraints (system extension/network filter approval flow, and known traffic classes not seen by Network Extensions).
   - Cloudflare WARP documents production modes that separate DNS filtering from broader network filtering (`DNS only`, `Traffic and DNS`, `Traffic only`), demonstrating deployable real-world split enforcement.

Implication for Cleanroom architecture:

- Keep one shared policy compiler and reason-code model.
- Implement backend-specific enforcement adapters:
  - Linux: Firecracker + nftables/eBPF enforcement.
  - macOS: Network Extension DNS/network filter adapter with explicit capability flags.
- Treat macOS hostname-level and DNS-level controls as capability-scoped and verify by conformance tests, not assumption.

## Key design inferences from the research

- `gondolin` has strong isolation ideas but is a larger multi-component platform. It is best treated as design inspiration for network and filesystem controls rather than a direct v1 embed.
- `matchlock` is the pragmatic base for local execution because it already provides:
  - backend abstraction
  - VM strategy in-process
  - command surface suitable for a wrapper binary
- `content-cache` and tokenizer-like secret mediation are cleaner to integrate as policy-aware external services than to reinvent in phase 1.

## Risks and unknowns still to validate

- Matchlock network enforcement edge cases (DNS interception and port-level blocking exactness).
- Image provenance verification depth (digest-only vs digest + signature/attestation enforcement).
- Operator UX for digest refresh cadence (how repos update pinned base image digests safely).
- Lockfile parser maturity by ecosystem.
- `content-cache` deployment topology for local-only vs remote-only workflows.
- macOS Network Extension edge cases where some traffic can bypass extension visibility (must be measured in our own conformance suite).
- macOS hostname filtering limits for traffic that does not use the expected networking APIs.

## Next concrete work after research

- Add capability handshake and launch validation path from compiled policy.
- Implement a first conformance subset:
  - default deny
  - explicit allow host/port
  - explicit deny precedence
  - stable reason codes in events
- Integrate `content-cache` in the backend provisioning flow before lockfile strict mode.
- Add `mise` detection and bootstrap strategy.
- Implement lockfile-restricted package allowlists.
- Add secret injection guardrails.
- Define cross-backend capability matrix (`linux-firecracker`, `darwin-network-extension`) and wire launch-time capability validation.
- Build a macOS proof-of-concept adapter using DNS proxy + network filter extensions, including explicit install/approval checks.
- Add cross-platform conformance tests for DNS and egress bypass paths (direct DNS, DoT, DoH, direct IP).
