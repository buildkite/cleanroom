# Cleanroom research notes

This document surveys agent sandbox tools and isolation backends to inform Cleanroom's design. It covers hosted providers, self-hosted tools, and the isolation technologies they use. The goal is practical: what exists, what works, and where Cleanroom fits.

## Landscape comparison

| Tool | Type | Isolation | Egress policy | Persistence | Image format | Boot time | SDK/CLI |
|---|---|---|---|---|---|---|---|
| E2B | Hosted | Firecracker microVM | None (full internet) | Ephemeral (pause/resume within session) | Custom templates | ~80-200ms | Python/JS/TS SDK |
| Daytona | Hosted (customer-managed option) | Not documented | None (full internet) | Persistent/stateful | Snapshots (OCI-compatible builder) | ~90ms creation | Python/TS/Go/Ruby SDK |
| Fly.io Sprites | Hosted (Fly.io infra) | Firecracker microVM | None (full internet) | Persistent, checkpoint/restore | No Docker/OCI | ~1-2s | CLI + Go/Python/Elixir SDKs |
| Modal | Hosted | gVisor (user-space kernel) | None (full internet) | Task-based/serverless | Python-defined environments | N/A | Python SDK |
| Tart | Self-hosted | Virtualization.framework (macOS) / libkrun (Linux) | None | Persistent | OCI images | ~5-10s | CLI (Swift) |
| Shuru | Self-hosted | Virtualization.framework (macOS only) | Deny-by-default (`--allow-net` opt-in) | Ephemeral (checkpoint reuse) | Alpine rootfs | Fast (checkpoint restore) | CLI (Rust) |
| SmolVM | Self-hosted | libkrun microVM | None | Ephemeral + persistent | OCI images | <200ms | CLI (Rust) |
| Matchlock | Self-hosted | Firecracker (Linux) / Virtualization.framework (macOS) | Allow-list (allow-all default, TLS MITM proxy) | Ephemeral | OCI images (EROFS) | <1s | CLI + Go/Python/TS SDK |
| Cleanroom | Self-hosted | Firecracker (Linux) / Virtualization.framework (macOS) | Deny-by-default, host:port allow rules in repo policy | Persistent sandboxes (Firecracker) | OCI images (digest-pinned, ext4) | N/A | CLI + ConnectRPC API |

For independent TTI (time-to-interactive) benchmarks across hosted sandbox providers, see [ComputeSDK benchmarks](https://www.computesdk.com/benchmarks/).

## Hosted providers

### E2B

[E2B](https://e2b.dev) runs Firecracker microVMs on hosted infrastructure. The open source infrastructure code means you can inspect the stack, but the product is a hosted API. Boot times are 80-200ms and custom templates let you pre-bake environments. Sandboxes are ephemeral with pause/resume within session limits.

No egress policy. Sandboxes get full internet access with no network filtering or credential isolation.

### Daytona

[Daytona](https://daytona.io) is open source with an option for customer-managed compute. The isolation mechanism is not publicly documented. Sandbox creation takes around 90ms and sandboxes persist across sessions. Images are built from snapshots or via a declarative builder that accepts OCI-compatible base images.

Daytona has configurable network limits but no repo-scoped egress policy or credential isolation.

### Fly.io Sprites

[Sprites](https://sprites.dev) runs on Fly.io infrastructure using Firecracker. Persistent, durable machines with checkpoint/restore. They do not use Docker or OCI images. Boot time is 1-2 seconds.

No egress policy. No network filtering.

### Modal

[Modal](https://modal.com) uses gVisor for isolation (a user-space kernel, not a full VM boundary). Environments are defined in Python. Modal has GPU support and a serverless execution model.

No egress policy.

### What hosted providers share

Every hosted provider gives sandboxes full internet access by default. None support repo-scoped egress policy or credential isolation. If you need to control what a sandbox can reach, you have to build that yourself on top of their APIs.

The [Browser Use thread](https://x.com/larsencc/status/2027225210412470668) is a good real-world example of how to work around this: isolate the agent in a micro-VM and hold credentials in a control plane that mediates all external access. The sandbox itself has zero secrets. This is the Pattern 2 architecture that Cleanroom also follows.

## Self-hosted tools

### Tart

[Tart](https://github.com/cirruslabs/tart) is a VM runner built on Virtualization.framework (macOS) and libkrun (Linux). It uses OCI-compatible images for distribution and supports both macOS and Linux guests. Persistent VMs with snapshot support. No egress policy, no network filtering, no sandbox policy model.

Tart is focused on CI workloads (Cirrus CI uses it). Not an agent sandbox tool, but relevant as prior art for OCI image distribution across VM backends.

### Shuru

[Shuru](https://github.com/superhq-ai/shuru) boots lightweight Linux VMs on macOS using Virtualization.framework. Each sandbox is ephemeral -- the rootfs resets on every run. Network access is denied by default and enabled with `--allow-net`. Checkpoints save disk state for reuse across runs. Uses vsock for guest communication and VirtioFS for directory mounts. macOS only (Apple Silicon), CLI only, written in Rust.

Shuru has the right default (deny-by-default network), but no policy engine, no per-host allowlisting, and no Linux support.

### SmolVM

[SmolVM](https://github.com/smol-machines/smolvm) is a VM runner using libkrun with OCI image support and sub-200ms boot. No policy engine, no networking control, no egress filtering. CLI only, written in Rust.

Useful if you just want a fast local VM. Not useful if you need any kind of network policy.

### Matchlock

[Matchlock](https://github.com/jingkaihe/matchlock) (validated at commit `9db058b9cddaf2769a201b5e67010e8b10f8b76e` on 2026-02-20) is the closest tool to Cleanroom. Same backend choices (Firecracker on Linux, Virtualization.framework on macOS), same general shape (CLI + SDK, OCI images, guest agent over vsock).

Matchlock has egress allow-listing and secret injection, which puts it ahead of everything else in this list. But there are design differences that matter:

- Matchlock defaults to allow-all when no allowlist is set. Cleanroom defaults to deny.
- Matchlock uses tag-based image references. Cleanroom requires digest-pinned refs for reproducibility.
- Matchlock's runtime policy is mutable (it auto-adds secret hosts at runtime). Cleanroom uses immutable compiled policy and fails launch on missing capabilities.
- Matchlock's TLS interception uses a MITM proxy. Cleanroom uses a gateway proxy model that avoids MITM.
- Matchlock's policy is runtime configuration. Cleanroom treats policy as code, scoped to the repository.

### Cleanroom positioning

Cleanroom is a self-hosted tool with deny-by-default egress, digest-pinned images, immutable compiled policy, and a gateway proxy (no MITM). Policy is defined per-repository in `cleanroom.yaml` and compiled before launch. The sandbox has no secrets -- credentials are held by the host control plane.

Early design references: [concept gist](https://gist.github.com/lox/cd5a74bee0c98e15c254e780bb73dd11), [earendil-works/gondolin](https://github.com/earendil-works/gondolin).

## Backends

The tools above use different isolation technologies. This section covers each backend independently, with notes on how it fits Cleanroom's enforcement model.

### Firecracker

[Firecracker](https://github.com/firecracker-microvm/firecracker) is a microVM monitor purpose-built for secure multi-tenant workloads. Minimal device model, fast boot, strong isolation boundary. Network model uses per-VM TAP interfaces and host firewall rules, which maps directly to deny-by-default enforcement.

Cleanroom uses Firecracker as the primary Linux backend. The backend is built around per-run TAP interfaces, host iptables FORWARD rules, generated machine JSON, and vsock guest-agent RPC.

Used by: E2B, Sprites, Matchlock, Cleanroom.

Current validated release: `v1.14.1` (published 2026-01-20). See also: [design](https://github.com/firecracker-microvm/firecracker/blob/main/docs/design.md), [FAQ](https://github.com/firecracker-microvm/firecracker/blob/main/FAQ.md), [production host setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md).

Related image tooling: [firecracker-rootfs-builder](https://github.com/WoodProgrammer/firecracker-rootfs-builder), [oci-image-executor](https://github.com/codecrafters-io/oci-image-executor).

### libkrun

[libkrun](https://github.com/containers/libkrun) is an embeddable VM library. It uses Hypervisor.framework on macOS and KVM on Linux. Guest and VMM share a security context. Network modes are TSI (transparent socket impersonation) or passt/gvproxy, which would require a different enforcement architecture than Cleanroom's TAP + host firewall model.

Decision: keep Firecracker as the default local Linux backend for v1. Both projects are actively maintained (libkrun `v1.17.4`, Firecracker `v1.14.1`). Swapping to libkrun would be a backend re-architecture, not a drop-in substitution. Re-evaluate libkrun as an optional secondary backend later, with explicit capability downgrades (for example, local macOS workflows with different enforcement guarantees).

Used by: Tart (Linux path), SmolVM.

Current validated release: `v1.17.4` (published 2026-02-18). See also: [releases](https://github.com/containers/libkrun/releases).

### Apple Virtualization.framework

Apple's [Virtualization.framework](https://developer.apple.com/documentation/virtualization) provides hardware-accelerated VMs on macOS (Apple Silicon and Intel). Cleanroom's `darwin-vz` backend uses a dedicated Swift helper binary for VM lifecycle and keeps policy/image/control-plane orchestration in Go.

Current status: per-run VMs only (no persistent sandboxes). Guest outbound networking is available via NAT, but allowlist egress filtering is not yet implemented. The backend enforces `network.default: deny` as a policy shape check and warns that `network.allow` entries are ignored. See [darwin-vz.md](backend/darwin-vz.md) for implementation details.

Used by: Tart, Matchlock (macOS path), Cleanroom (macOS path).

### Docker / containers

Container isolation (Linux namespaces, cgroups, seccomp) is the weakest boundary in this comparison. A kernel vulnerability in the container can compromise the host. Containers do not provide a hardware virtualization boundary.

Docker is widely used for developer tooling and CI, and container images (OCI) are the standard packaging format. Cleanroom uses OCI images as sandbox base images but runs them inside microVMs rather than containers for a stronger isolation boundary.

Used by: Daytona.

### Cross-platform filtering

Validation snapshot date: 2026-02-17.

1. There is no single kernel-level filtering substrate shared across Linux and macOS.
   - Linux path can use [eBPF](https://docs.ebpf.io/linux/) and nftables.
   - macOS path should use Network Extension providers (for example [`NEDNSProxyProvider`](https://developer.apple.com/documentation/networkextension/nednsproxyprovider) / network filter provider model), not Linux packet-filter assumptions. Apple's [TN3165](https://developer.apple.com/documentation/technotes/tn3165-packet-filter-is-not-api) confirms that packet filter is not a supported API.
2. DNS resolver-level filtering is practical on both platforms, but with different enforcement primitives.
   - Linux: resolver control + egress firewall policy.
   - macOS: DNS proxy payload / DNS proxy extension model.
3. Prior art for reliable macOS filtering exists and is active.
   - [LuLu](https://objective-see.org/products/lulu.html) documents a Network Extension-based firewall model and calls out operational constraints (system extension/network filter approval flow, and known traffic classes not seen by Network Extensions).
   - [Cloudflare WARP](https://developers.cloudflare.com/cloudflare-one/team-and-resources/devices/warp/configure-warp/warp-modes/) documents production modes that separate DNS filtering from broader network filtering ("DNS only", "Traffic and DNS", "Traffic only"), demonstrating deployable real-world split enforcement.

Implication for Cleanroom architecture:

- Keep one shared policy compiler and reason-code model.
- Implement backend-specific enforcement adapters:
  - Linux: Firecracker + nftables/eBPF enforcement.
  - macOS: Network Extension DNS/network filter adapter with explicit capability flags.
- Treat macOS hostname-level and DNS-level controls as capability-scoped and verify by conformance tests, not assumption.

## Caching and secret mediation

### content-cache

[content-cache](https://github.com/wolfeidau/content-cache) is a caching proxy for package registries and git hosting. Cleanroom uses it as the mediation layer between the sandbox and upstream registries. Package and git egress is routed through content-cache on the host side, so the sandbox never talks directly to upstream. This gives Cleanroom a single point for allowlist enforcement, caching, and audit logging of registry traffic.

content-cache also enables offline/hermetic builds when only pre-warmed cached artifacts are permitted.

### tokenizer

[tokenizer](https://github.com/superfly/tokenizer) is Fly.io's credential proxy. It holds secrets and injects them into outbound requests on behalf of callers that never see the plaintext values. Cleanroom follows the same pattern: secret IDs are declared in policy, resolved at runtime by the host control plane, and injected on the upstream leg of proxied requests. The sandbox never has access to secret material.

Key properties of this model:

- No plaintext secrets in policy files, command arguments, or guest environment.
- Secret injection is scoped by destination host -- a secret bound to `api.github.com` cannot be injected into requests to other hosts.
- All injection events are logged with secret ID and destination, never with secret values.

## Research conclusions

1. Use Firecracker as the local sandbox backend (inspired by Matchlock patterns). See [backend/firecracker.md](backend/firecracker.md) for implementation details.
2. Use `content-cache` as the package/registry and git mediation layer.
3. Use a tokenizer-like secret-injection model with host-scoped policy and no plaintext propagation.
4. Keep CLI first: `cleanroom exec` as primary entrypoint and command pattern.

