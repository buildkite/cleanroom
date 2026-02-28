# Isolation model

Workloads run in a Linux microVM (`firecracker` on Linux, `darwin-vz` on macOS).

## Network enforcement

- `firecracker` enforces policy egress allowlists with per-sandbox TAP interfaces and iptables rules.
- `darwin-vz` currently requires `network.default: deny`, ignores `network.allow` entries, and provides guest networking without egress filtering. A warning is printed during execution.

## Filesystem persistence

- `firecracker`: rootfs writes persist across executions within a sandbox and are discarded on sandbox termination. Rootfs copy uses clone/reflink when available, with copy fallback.
- `darwin-vz`: each command runs in a fresh VM with a fresh rootfs copy. Writes are discarded after each run.

## Observability

Per-run timing metrics are written to `run-observability.json`:
- rootfs prep
- network setup
- VM ready
- command runtime
- total
