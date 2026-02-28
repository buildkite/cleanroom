# Firecracker backend

**Spec reference:** spec.md section 7 (backend abstraction), section 12 (build plan)

## Summary

Cleanroom's primary Linux backend uses [Firecracker](https://github.com/firecracker-microvm/firecracker) microVMs with per-sandbox TAP networking and host iptables enforcement. Each sandbox gets a dedicated TAP interface, generated machine JSON, and a vsock guest-agent for command execution.

Firecracker is purpose-built for secure multi-tenant workloads with a minimal device model. Its network model (TAP + host firewall) maps directly to Cleanroom's deny-by-default enforcement.

## Scope

- Linux-only local backend using Firecracker + KVM.
- Enforce `CompiledPolicy` only (no runtime repo policy reload).
- Deny-by-default egress with explicit host/port allowlist.
- Route package and git egress through `content-cache`.
- Keep secret values out of guest env and policy files.

## Implementation slices

1. Slice A: minimal Firecracker runner -- create backend adapter package and run lifecycle. Boot VM, run command over vsock, collect exit code/stdout/stderr.
2. Slice B: deterministic networking -- add TAP/subnet allocator + nftables setup/teardown. Enforce default deny and exact host/port allowlist (no registries yet).
3. Slice C: registry and git mediation -- start/attach `content-cache`. Rewrite package/git traffic through cache endpoint and emit deny reasons for bypass attempts.
4. Slice D: secret proxy -- add tokenizer-style host-scoped injection path. Enforce `secret_scope_violation` and keep secret values out of guest-visible env/args.
5. Slice E: conformance and hardening -- implement backend capability handshake. Add conformance suite from spec.md section 14 before backend marked supported.

## Capabilities

Current capability values (visible in `cleanroom doctor --json`):

- `exec.streaming=true`
- `sandbox.persistent=true`
- `sandbox.file_download=true`
- `network.default_deny=true`
- `network.allowlist_egress=true`
- `network.guest_interface=true`

## Host requirements

- `/dev/kvm` available and writable
- Firecracker binary installed
- `mkfs.ext4` for OCI-to-ext4 materialization
- `sudo -n` access for `ip`, `iptables`, `sysctl`

## Related

- [darwin-vz.md](darwin-vz.md) -- macOS backend
- [isolation.md](../isolation.md) -- enforcement and persistence details
- [research.md](../research.md) -- backend evaluation and comparison
