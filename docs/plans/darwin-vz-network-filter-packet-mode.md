# Darwin VZ Network Filter Packet Mode Plan

## Goal

Define the supported host-side filtering model for `darwin-vz` when the runtime
uses `vmnet-shared`.

Near-term target:

- keep `vmnet-shared` as the network identity mechanism
- stop relying on host PID and process-path flow attribution
- replace the current flow-based content filter with packet-layer filtering
- preserve the existing user-facing policy surface (`network.default`,
  `network.allow`) while changing how it is compiled for enforcement

This document is a follow-on to
[`darwin-vz-vmnet-mode.md`](./darwin-vz-vmnet-mode.md). That document explains
how Cleanroom gets stable vmnet guest identity. This document explains how that
identity should be filtered.

## Current State

Validated locally on April 4, 2026:

- `darwin-vz` is running in `vmnet-shared` mode on supported hosts
- the helper returns vmnet network metadata including subnet, guest IP, gateway
  IP, and prefix length
- the network daemon publishes the expected allow/deny policy
- the macOS support app activates the system extension and enables
  `NEFilterManager`
- the current provider starts successfully
- the current provider does not receive any VM traffic flows to evaluate

Observed behavior:

- a deny-by-default policy allowing only `github.com:443` still allows
  `buildkite.com:443`
- provider status advances from `start` to `heartbeat`
- provider logs show `startFilter invoked`
- provider logs do not show any `handleNewFlow` activity during vmnet guest
  traffic

## Problem Statement

The current filter implementation is built around `NEFilterDataProvider` and
host process identity:

- `NEFilterProviderConfiguration.filterSockets = true`
- `CleanroomFilterDataProvider` subclasses `NEFilterDataProvider`
- flow evaluation is scoped to the Virtualization XPC process path
- compiled policy is keyed to host PIDs

That model is not a good fit for vmnet guest traffic.

Apple documents `NEFilterDataProvider` as receiving `NEFilterFlow` objects for
network connections opened by applications running on the device. Cleanroom's
vmnet guest traffic is not presenting as host application flows in the way this
provider expects.

The result is a structural mismatch:

- vmnet gives Cleanroom guest network identity
- the current filter asks the system for host app flow identity

## Verified Constraints

- Apple recommends Network Extension content filtering for supported filtering
  products, not `pf`
- on macOS, `NEFilterPacketProvider` receives Layer 2 packets
- `NEFilterDataProvider` is flow-based and primarily oriented around app-opened
  connections
- `vmnet-shared` already gives Cleanroom the guest network metadata needed to
  identify a sandbox at the network layer

Operational implications:

- guest IP / subnet is the stable key for darwin-vz filtering
- host PID / process path is only a launch/runtime detail
- a vmnet-capable darwin-vz path should be filtered as network traffic, not as
  app socket flows

## Decision

For `darwin-vz` on `vmnet-shared`, Cleanroom should move to packet-layer
filtering:

1. Replace `NEFilterDataProvider` with `NEFilterPacketProvider`.
2. Treat vmnet guest IP identity as the filter key.
3. Compile hostname-based policy into guest-scoped packet rules.
4. Keep the support app, system extension, and network daemon architecture.
5. Stop investing in PID-scoped flow filtering for darwin-vz.

This does not require changing the user-facing repository policy syntax.

## Proposed Architecture

### 1. Runtime Identity

The darwin-vz helper already reports:

- vmnet subnet CIDR
- guest IPv4
- gateway IPv4
- prefix length

That metadata should become the identity handed to the network-filter daemon.

The daemon should track policy by guest network identity, not by process:

- guest IP
- optional subnet
- optional future MAC address

### 2. Provider Type

Use `NEFilterPacketProvider` for darwin-vz enforcement on macOS.

Reason:

- it operates at the packet layer
- it is the Network Extension mechanism Apple describes for packet filtering on
  macOS
- it aligns with vmnet guest identity
- it avoids depending on host process flow attribution that has already failed

### 3. Policy Compilation

User-facing policy should stay backend-neutral:

- `network.default: allow|deny`
- `network.allow`

Compiled darwin-vz packet policy should become:

- guest identity selector
- default action
- remote IP / prefix allow rules
- protocol and port constraints where applicable

This is a compilation problem, not a user policy problem.

### 4. DNS Strategy

This is the main new design requirement.

Packet filtering does not naturally operate on hostnames. To preserve
hostname-based policy, Cleanroom needs a compilation strategy from
`host: github.com` to remote IP rules.

Initial approach:

1. Resolve allowed hostnames in the network-filter daemon.
2. Publish resolved IP rules to the provider keyed by guest identity.
3. Refresh those resolutions on a short TTL-bound cadence.

Follow-on improvement:

- watch guest DNS answers and maintain a tighter guest-specific hostname to IP
  mapping

The initial approach is simpler and testable, but it will need explicit
handling for CDNs, TTL churn, and transient DNS failure.

### 5. App and Daemon Boundary

No product-boundary change is required.

Keep:

- `Cleanroom.app` as the Apple API bridge for system extension activation and
  `NEFilterManager`
- the root-owned network-filter daemon as the source of truth for policy and
  status
- `cleanroom network ...` as the user-facing control surface

Change only:

- provider implementation
- compiled policy shape
- doctor/status wording to reflect packet-mode enforcement

## Policy Model Changes

The current darwin-vz filter policy model is process-scoped:

- `process_rules[pid]`

That should be replaced with guest-network scoping, for example:

- `guest_rules[guest_ip]`

Each guest-scoped policy should contain:

- default action
- allowed remote IP ranges
- allowed ports / protocols
- timestamps and generation markers for refresh/debugging

The host PID can remain in observability, but not in enforcement.

## Non-Goals

- no attempt to recreate Linux TAP + iptables semantics exactly
- no reintroduction of `pf`
- no broad user-facing policy language change
- no IPv6 design in the first slice
- no port-forwarding design in this document

## Testing Plan

### Required

1. Packet provider starts and receives vmnet guest traffic.
2. Deny-by-default policy allowing only `github.com:443` blocks
   `buildkite.com:443`.
3. Guest identity in the daemon matches helper-reported vmnet guest metadata.
4. Provider status records packet activity, not only heartbeat.

### Recommended

1. Explicit test proving the old flow provider does not see vmnet guest traffic.
2. Packet filtering test on the default shared vmnet network.
3. Packet filtering test on a custom RFC1918 vmnet subnet.
4. DNS-refresh behavior test for short-lived host resolution changes.

## Open Questions

- How aggressive should hostname resolution refresh be?
- Should guest DNS traffic be modeled specially in the first slice?
- When a hostname resolves to many addresses, should Cleanroom allow all
  returned addresses or a narrower active subset?
- Does the first slice need UDP beyond DNS?
- Should the provider key rules by guest IP only, or by subnet plus guest IP?

## Ordered Experiments

These are the next concrete diagnostics to run before making another design
decision:

1. Run `vmnet-shared` on an explicit RFC1918 subnet, currently
   `10.233.0.0/16`.
   Reason:
   - reduce ambiguity from the default `192.168.64.0/24` shared network
   - make guest-address matches visually obvious in packet traces

2. Pin the shared network to the default host uplink with
   `vmnet_network_configuration_set_external_interface`, currently expected to
   be `en0`.
   Reason:
   - remove routing-table-dependent uplink selection from the experiment

3. Reduce vmnet-managed services during tracing:
   - disable DNS proxy
   - disable router advertisements
   - disable NAT66
   Reason:
   - reduce IPv6 and DNS noise in packet traces
   - keep NAT44 enabled so outbound IPv4 egress remains available

4. Run one diagnostic pass with NAT44 disabled.
   Reason:
   - determine whether shared-mode NAT44 is the exact point where guest egress
     stops being visible as `guest_ip -> remote_ip`
   Expected:
   - outbound internet may stop working
   - this run is about packet visibility, not a passing product configuration

5. If packet visibility is still post-NAT, add a host endpoint on the logical
   network with `vmnet_interface_start_with_network` and inspect packets using
   `vmnet_read`.
   Reason:
   - Apple exposes packet I/O on the logical network itself
   - this is the most promising supported place to observe pre-NAT guest
     traffic without leaving `vmnet`

### Results So Far

Validated locally on April 4, 2026:

1. Explicit RFC1918 subnet + explicit uplink + reduced vmnet services:
   - configuration:
     - subnet `10.233.0.0/16`
     - external interface `en0`
     - `disable_nat66 = true`
     - `disable_dns_proxy = true`
     - `disable_router_advertisement = true`
   - result:
     - helper-reported guest identity changed as expected to `10.233.0.2/16`
     - `darwin-vz-config.json` recorded:
       - `network_subnet_cidr = 10.233.0.0/16`
       - `network_guest_ip = 10.233.0.2`
       - `network_gateway_ip = 10.233.0.1`
     - `NEFilterPacketProvider` still only observed outbound IPv4 egress after
       translation on host `en0`, for example:
       - `192.168.1.45 -> public-ip:443`
     - provider traces still did not contain packets with source
       `10.233.0.2`

2. NAT44 disabled:
   - result:
     - `vmnet_network_create(...)` failed
     - helper returned `create vmnet shared network: general failure`
     - host log recorded:
       - `interface creation failed`
       - `vmnet_network_create: _NETRBCreateNetwork`
   - conclusion:
     - `disable_nat44` is not a viable runtime configuration for this path on
       the current host

3. Host endpoint on the same logical vmnet network:
   - implementation:
     - started an additional interface with
       `vmnet_interface_start_with_network`
     - read packets with `vmnet_read`
   - result:
     - host-endpoint trace captured only gateway-side traffic such as:
       - IPv4 mDNS from `10.233.0.1 -> 224.0.0.251:5353`
       - link-local IPv6 control traffic
     - it did not capture packets with guest source `10.233.0.2`
     - it did not capture guest egress to remote public IPs
   - conclusion:
     - the extra host endpoint behaves like another participant on the logical
       network, not a packet tap for guest transit traffic

Current conclusion:

- `vmnet-shared` guest identity works
- `NEFilterPacketProvider` does not see usable pre-NAT guest egress identity
- `vmnet_interface_start_with_network` also does not expose guest transit
  packets in the form needed for guest-IP-based egress enforcement
- further work on the current guest-IP matching model is unlikely to change the
  result without a different visibility point

For the next candidate visibility point, see
[`darwin-vz-filehandle-network-backend.md`](./darwin-vz-filehandle-network-backend.md).

## Sources

- [Apple Developer: NEFilterDataProvider](https://developer.apple.com/documentation/NetworkExtension/NEFilterDataProvider)
- [Apple Developer: TN3165: Packet Filter is not API](https://developer.apple.com/documentation/technotes/tn3165-packet-filter-is-not-api)
- [Apple Developer WWDC19: Network Extensions for the Modern Mac](https://developer.apple.com/videos/play/wwdc2019/714/)
- [Apple Developer WWDC25: Filter and tunnel network traffic with NetworkExtension](https://developer.apple.com/videos/play/wwdc2025/234/)
- [Apple containerization README](https://github.com/apple/containerization)
