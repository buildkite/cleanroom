# Isolation model

Workloads run in a Linux microVM (`firecracker` on Linux, `darwin-vz` on macOS).

## Network model and enforcement

- `firecracker` creates a dedicated TAP interface and host/guest IP pair per sandbox. It enforces policy egress allowlists with host-side iptables rules, and the host can identify a sandbox by its guest IP on that TAP-backed network.
- `firecracker` learns hostname-based allowed destinations from observed DNS answers and then authorizes destination IP:port pairs. Literal IPv4 allow rules are installed directly. Current hostname-based rules therefore do not distinguish co-hosted services that share the same IP:port.
- `darwin-vz` uses `filehandle` networking with a Cleanroom-owned guest gateway for TCP egress filtering. The host does not get a Firecracker-style per-sandbox TAP device or host-visible guest IP identity.
- `darwin-vz` also learns allowed destinations from observed DNS answers and then authorizes destination IP:port pairs in the filehandle gateway. Current hostname-based rules therefore do not distinguish co-hosted services that share the same IP:port.

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
