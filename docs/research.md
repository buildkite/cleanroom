# Cleanroom research notes

This document surveys agent sandbox tools to understand the landscape and inform Cleanroom's design. It covers hosted providers, self-hosted tools, and how each handles isolation, networking, and policy. The goal is practical: what exists, what works, and where Cleanroom fits.

## Landscape comparison

| Tool | Type | Isolation | Egress policy | Persistence | Image format | Boot time | SDK/CLI |
|---|---|---|---|---|---|---|---|
| E2B | Hosted | Firecracker microVM | None (full internet) | Ephemeral (pause/resume within session) | Custom templates | ~80-200ms | Python/JS/TS SDK |
| Daytona | Hosted (customer-managed option) | Container | None (full internet) | Persistent/stateful | Docker images | ~90ms creation | Python/TS SDK |
| Fly.io Sprites | Hosted (Fly.io infra) | Firecracker microVM | None (full internet) | Persistent, checkpoint/restore | No Docker/OCI | ~1-2s | CLI + Go/Python/Elixir SDKs |
| Modal | Hosted | gVisor (user-space kernel) | None (full internet) | Task-based/serverless | Python-defined environments | N/A | Python SDK |
| SmolVM | Self-hosted | libkrun microVM | None | Ephemeral + persistent | OCI images | <200ms | CLI (Rust) |
| Matchlock | Self-hosted | Firecracker (Linux) / Virtualization.framework (macOS) | Allow-list (allow-all default, TLS MITM proxy) | Ephemeral | OCI images (EROFS) | <1s | CLI + Go/Python/TS SDK |
| Cleanroom | Self-hosted | Firecracker (Linux) / Virtualization.framework (macOS) | Deny-by-default, host:port allow rules in repo policy | Persistent sandboxes (Firecracker) | OCI images (digest-pinned, ext4) | N/A | CLI + ConnectRPC API |

## Hosted providers

### E2B

[E2B](https://e2b.dev) runs Firecracker microVMs on hosted infrastructure. The open source infrastructure code means you can inspect the stack, but the product is a hosted API. Isolation is strong (Firecracker), boot times are fast (80-200ms), and custom templates let you pre-bake environments. Sandboxes are ephemeral with pause/resume within session limits.

No egress policy. Sandboxes get full internet access with no network filtering or credential isolation.

### Daytona

[Daytona](https://daytona.io) is container-based and open source, with an option for customer-managed compute. It emphasizes speed (90ms sandbox creation) and stateful sandboxes that persist across sessions. Standard Docker images.

Like E2B, there is no egress policy or credential isolation. Sandboxes have unrestricted network access.

### Fly.io Sprites

[Sprites](https://sprites.dev) runs on Fly.io infrastructure using Firecracker. The pitch is "computers, not sandboxes" -- persistent, durable machines with checkpoint/restore. They do not use Docker or OCI images. Boot time is 1-2 seconds.

No egress policy. No network filtering.

### Modal

[Modal](https://modal.com) uses gVisor for isolation, which is a user-space kernel rather than a full VM boundary. This is a weaker isolation boundary than Firecracker. Environments are defined in Python. Modal has GPU support and a serverless execution model.

No egress policy.

### What hosted providers share

Every hosted provider gives sandboxes full internet access by default. None of them support repo-scoped egress policy or credential isolation. If you need to control what a sandbox can reach, you have to build that yourself on top of their APIs.

E2B and Sprites use Firecracker, which provides strong isolation. Modal's gVisor boundary is weaker. Daytona uses containers, which is the weakest isolation of the group.

The [Browser Use thread](https://x.com/larsencc/status/2027225210412470668) is a good real-world example of how to work around this: isolate the agent in a micro-VM and hold credentials in a control plane that mediates all external access. The sandbox itself has zero secrets. This is the Pattern 2 architecture that Cleanroom also follows.

## Self-hosted tools

### SmolVM

[SmolVM](https://github.com/smol-machines/smolvm) is the simplest tool in this space. It is a VM runner using libkrun (Hypervisor.framework on macOS, KVM on Linux) with OCI image support and sub-200ms boot. No policy engine, no networking control, no egress filtering. CLI only, written in Rust.

Useful if you just want a fast local VM. Not useful if you need any kind of network policy.

### Matchlock

[Matchlock](https://github.com/jingkaihe/matchlock) is the closest tool to Cleanroom. Same backend choices (Firecracker on Linux, Virtualization.framework on macOS), same general shape (CLI + SDK, OCI images, guest agent over vsock).

Matchlock has egress allow-listing and secret injection, which puts it ahead of everything else in this list. But there are design differences that matter:

- Matchlock defaults to allow-all when no allowlist is set. Cleanroom defaults to deny.
- Matchlock uses tag-based image references. Cleanroom requires digest-pinned refs for reproducibility.
- Matchlock's runtime policy is mutable (it auto-adds secret hosts at runtime). Cleanroom uses immutable compiled policy and fails launch on missing capabilities.
- Matchlock's TLS interception uses a MITM proxy. Cleanroom uses a gateway proxy model that avoids MITM.
- Matchlock's policy is runtime configuration. Cleanroom treats policy as code, scoped to the repository.

### Cleanroom positioning

Cleanroom is a self-hosted tool with deny-by-default egress, digest-pinned images, immutable compiled policy, and a gateway proxy (no MITM). Policy is defined per-repository in `cleanroom.yaml` and compiled before launch. The sandbox has no secrets -- credentials are held by the host control plane.

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

### Agent sandbox architecture

- [Browser Use: How We Built Secure, Scalable Agent Sandbox Infrastructure](https://x.com/larsencc/status/2027225210412470668) -- Pattern 2 (isolate the agent) with Unikraft micro-VMs and a credential-holding control plane. Validates the gateway/proxy model where the sandbox has zero secrets and all external access is mediated by the host.

### Cross-platform enforcement references

- [eBPF Docs: eBPF on Linux](https://docs.ebpf.io/linux/)
- [Apple Developer: `NEDNSProxyProvider`](https://developer.apple.com/documentation/networkextension/nednsproxyprovider)
- [Apple Technical Note TN3165](https://developer.apple.com/documentation/technotes/tn3165-packet-filter-is-not-api)
- [Apple Support: DNS Proxy payload settings](https://support.apple.com/guide/deployment/dns-proxy-payload-settings-dep500f65271/web)
- [Objective-See LuLu](https://objective-see.org/products/lulu.html)
- [Cloudflare WARP modes](https://developers.cloudflare.com/cloudflare-one/team-and-resources/devices/warp/configure-warp/warp-modes/)

## What maps directly into v1 Cleanroom

1. Use Firecracker as the local sandbox backend (inspired by Matchlock patterns).
2. Use `content-cache` as the package/registry and git mediation layer.
3. Use a tokenizer-like secret-injection model with host-scoped policy and no plaintext propagation.
4. Keep CLI first: `cleanroom exec` as primary entrypoint and command pattern.

## Firecracker backend proposal

Build our own backend in Cleanroom, but reuse Matchlock techniques that are already proven in practice.

### Scope for initial backend

- Linux-only local backend using Firecracker + KVM.
- Enforce `CompiledPolicy` only (no runtime repo policy reload).
- Deny-by-default egress with explicit host/port allowlist.
- Route package and git egress through `content-cache`.
- Keep secret values out of guest env and policy files.

### Incremental implementation plan

1. Slice A: minimal Firecracker runner -- create backend adapter package and run lifecycle. Boot VM, run command over vsock, collect exit code/stdout/stderr.
2. Slice B: deterministic networking -- add TAP/subnet allocator + nftables setup/teardown. Enforce default deny and exact host/port allowlist (no registries yet).
3. Slice C: registry and git mediation -- start/attach `content-cache`. Rewrite package/git traffic through cache endpoint and emit deny reasons for bypass attempts.
4. Slice D: secret proxy -- add tokenizer-style host-scoped injection path. Enforce `secret_scope_violation` and keep secret values out of guest-visible env/args.
5. Slice E: conformance and hardening -- implement backend capability handshake. Add conformance suite from `SPEC.md` section 14 before backend marked supported.

## libkrun vs Firecracker

Decision: keep Firecracker as the default local Linux backend for v1.

Both projects are actively maintained (libkrun `v1.17.4`, Firecracker `v1.14.1`). The decision comes down to fit:

- Firecracker is purpose-built for secure multi-tenant workloads with a minimal device model. Its network model (TAP + host firewall) maps directly to Cleanroom's deny-by-default enforcement.
- libkrun is an embeddable library where guest and VMM share a security context. Its network modes (TSI or passt/gvproxy) would require a different enforcement architecture.
- Cleanroom's current backend is already built around Firecracker's model: per-run TAP, host firewall FORWARD rules, generated machine JSON, vsock guest-agent RPC.

Swapping to libkrun would be a backend re-architecture, not a drop-in substitution. Re-evaluate libkrun as an optional secondary backend later, with explicit capability downgrades (for example, local macOS workflows with different enforcement guarantees).

## Cross-platform filtering findings

Validation snapshot date: 2026-02-17.

1. There is no single kernel-level filtering substrate we can share across Linux and macOS.
   - Linux path can use eBPF and nftables.
   - macOS path should use Network Extension providers (for example DNS proxy provider / network filter provider model), not Linux packet-filter assumptions.
2. DNS resolver-level filtering is practical on both platforms, but with different enforcement primitives.
   - Linux: resolver control + egress firewall policy.
   - macOS: DNS proxy payload / DNS proxy extension model.
3. Prior art for reliable macOS filtering exists and is active.
   - LuLu documents a Network Extension-based firewall model and calls out operational constraints (system extension/network filter approval flow, and known traffic classes not seen by Network Extensions).
   - Cloudflare WARP documents production modes that separate DNS filtering from broader network filtering ("DNS only", "Traffic and DNS", "Traffic only"), demonstrating deployable real-world split enforcement.

Implication for Cleanroom architecture:

- Keep one shared policy compiler and reason-code model.
- Implement backend-specific enforcement adapters:
  - Linux: Firecracker + nftables/eBPF enforcement.
  - macOS: Network Extension DNS/network filter adapter with explicit capability flags.
- Treat macOS hostname-level and DNS-level controls as capability-scoped and verify by conformance tests, not assumption.
