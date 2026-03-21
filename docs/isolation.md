# Isolation model

Workloads run in a Linux microVM (`firecracker` on Linux, `darwin-vz` on macOS).

## Network model and enforcement

- `firecracker` creates a dedicated TAP interface and host/guest IP pair per sandbox. It enforces policy egress allowlists with host-side iptables rules, and the host can identify a sandbox by its guest IP on that TAP-backed network.
- `darwin-vz` currently attaches a NAT-backed virtual NIC through `Virtualization.framework`. Guests get outbound networking and can reach the host gateway through the NAT host address, but the host does not get a Firecracker-style per-sandbox TAP device or host-visible guest IP identity.
- `darwin-vz` currently requires `network.default: deny`, ignores `network.allow` entries, and provides no host-side egress allowlist enforcement. A warning is printed during execution.
- Planned work for vmnet-backed `darwin-vz` networking is tracked in [plans/darwin-vz-vmnet-mode.md](plans/darwin-vz-vmnet-mode.md).

## Filesystem persistence

- `firecracker`: rootfs writes persist across executions within a sandbox and are discarded on sandbox termination. Rootfs copy uses clone/reflink when available, with copy fallback.
- `darwin-vz`: rootfs writes persist across executions within a sandbox and are discarded on sandbox termination.

## Observability

Per-execution timing metrics are written to `execution-observability.json`:
- rootfs prep
- network setup
- VM ready
- command runtime
- total
