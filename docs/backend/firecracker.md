# Firecracker backend

**Spec reference:** spec.md section 7 (backend abstraction), section 12 (build plan)

## Summary

Cleanroom's primary Linux backend uses [Firecracker](https://github.com/firecracker-microvm/firecracker) microVMs with per-sandbox TAP networking, a host-side trusted DNS forwarder, and host firewall enforcement. Each sandbox gets a dedicated TAP interface, generated machine JSON, and a vsock guest-agent for command execution.

Firecracker is purpose-built for secure multi-tenant workloads with a minimal device model. Its network model (TAP + host firewall) maps directly to Cleanroom's deny-by-default enforcement.

## Scope

- Linux-only local backend using Firecracker + KVM.
- Enforce `CompiledPolicy` only (no runtime repo policy reload).
- Deny-by-default egress with explicit allow rules.
- Route git egress through the shared host gateway, with embedded
  `content-cache` for Git and OCI transport caching.
- Keep secret values out of guest env and policy files.

## Network Model

Firecracker networking is built around a dedicated per-sandbox TAP device on the host:

- each sandbox gets a unique host IP / guest IP pair on that TAP-backed subnet
- the host can identify the sandbox by guest source IP
- guest DNS is redirected to a host-side resolver built on `miekg/dns`
- the shared host gateway is exposed inside the guest as `gateway.cleanroom.internal`
- DNS answers are observed per sandbox/guest IP and projected into dynamic `ipset`-backed allow rules
- host-side iptables rules enforce default-deny egress, with established flows surviving DNS TTL expiry
- gateway access is bound to the sandbox's TAP/IP identity rather than a helper-managed token

Current hostname-based allow rules are enforced from observed DNS answers plus
destination IP:port. Firecracker does not currently distinguish co-hosted
services that share the same IP:port.

This is materially different from the current `darwin-vz` backend, which uses helper-managed NAT networking and does not expose a host-visible per-sandbox guest IP. See [darwin-vz.md](darwin-vz.md) for the current macOS model.

## Implementation slices

1. Slice A: minimal Firecracker runner -- create backend adapter package and run lifecycle. Boot VM, run command over vsock, collect exit code/stdout/stderr.
2. Slice B: deterministic networking -- add TAP/subnet allocator + nftables setup/teardown. Enforce default deny and host/port allowlist (no registries yet).
3. Slice C: registry and git mediation -- start/attach embedded
   `content-cache`. Rewrite git traffic through cache-backed gateway routes,
   serve OCI registry pulls through the same cache layer, and emit deny reasons
   for bypass attempts. Broader package-manager rewrites remain follow-up work.
4. Slice D: secret proxy -- add tokenizer-style host-scoped injection path. Enforce `secret_scope_violation` and keep secret values out of guest-visible env/args.
5. Slice E: conformance and hardening -- implement backend capability handshake. Add conformance suite from spec.md section 14 before backend marked supported.

## Capabilities

Current capability values (visible in `cleanroom doctor --json`):

- `exec.streaming=true`
- `sandbox.file_download=true`
- `network.default_deny=true`
- `network.allowlist_egress=true`
- `dns_control_or_equivalent=true`
- `network.guest_interface=true`

## Host requirements

- `/dev/kvm` available and writable
- Firecracker binary installed
- `mkfs.ext4` for OCI-to-ext4 materialization
- `debugfs` for runtime rootfs preparation
- `sudo -n` access for privileged host networking (`ip`, `ipset`, `iptables`, `sysctl`)

## Related

- [darwin-vz.md](darwin-vz.md) -- macOS backend
- [isolation.md](../isolation.md) -- enforcement and persistence details
- [research.md](../research.md) -- backend evaluation and comparison
- [../plans/snapshot-restore-fork.md](../plans/snapshot-restore-fork.md) -- proposed snapshot and create-from-snapshot design
