# Darwin VZ Vmnet Mode Plan

## Goal

Add an opt-in vmnet-backed networking mode for `darwin-vz` that moves the backend closer to Firecracker's per-sandbox network identity model without introducing LAN bridging.

Target outcome:

- one host-reachable guest IP per sandbox
- no LAN presence
- outbound internet access retained
- groundwork for future host access to guest-bound ports

This document is a plan only. It does not describe implemented behavior.

## Why

Current `darwin-vz` networking is helper-managed NAT:

- guest outbound networking is available
- `network.allow` is not enforced
- the host does not have a Firecracker-style per-sandbox TAP device
- the host does not have a stable per-sandbox guest IP identity
- gateway access uses a NAT host address and scope-token headers

That differs materially from Firecracker, where each sandbox has:

- a dedicated TAP device
- a unique host IP / guest IP pair
- host-side firewall rules keyed to sandbox network identity

Vmnet mode is meant to close the identity and host-reachability gap, not to claim exact TAP parity with Linux.

## Non-Goals

- no implementation in this slice
- no claim of exact Linux TAP or iptables parity
- no LAN bridging
- no public API changes for port publishing in the first slice

## Proposed Mode

Introduce an opt-in `darwin-vz` network mode based on vmnet shared-mode logical networks.

Recommended first mode:

- `vmnet-shared`

Characteristics:

- one logical vmnet network per sandbox
- one private `/30` subnet per sandbox
- host IP is the second address in that subnet
- guest IP is the only assignable guest address in that subnet
- outbound internet access remains available through vmnet shared-mode NAT
- no dependence on the physical LAN

Why `/30`:

- it fits the one-host / one-guest topology exactly
- it keeps sandbox identity simple and deterministic
- it minimizes accidental sandbox-to-sandbox overlap

## Constraints

- `VZVmnetNetworkDeviceAttachment` is available only on macOS 26+
- vmnet-backed networking requires additional authorization beyond the current virtualization entitlement
- the vmnet network object must be created in the same process that attaches it to the VM

Operational implication:

- the Swift helper must own vmnet network creation
- Go can plan network identity, but it cannot create the vmnet object on behalf of the helper

## Proposed Architecture

### 1. Runtime Config

Add darwin-specific runtime config for networking:

- `backends.darwin-vz.network.mode`
- `backends.darwin-vz.network.subnet_pool`
- `backends.darwin-vz.network.dns`
- optional `backends.darwin-vz.network.external_interface`

Initial default should remain `nat` until vmnet mode is proven.

### 2. Network Planning

Before `StartVM`, the Go backend allocates a sandbox network plan:

- subnet CIDR
- host IP
- guest IP
- prefix length
- deterministic guest MAC

This plan is passed to the helper as part of the control request and persisted in sandbox state.

### 3. Helper Networking

In vmnet mode, the helper:

- creates a vmnet shared-mode logical network
- applies the requested subnet
- attaches the VM NIC with `VZVmnetNetworkDeviceAttachment`
- returns actual network identity in `StartVM` response

The current NAT attachment remains as the fallback path.

### 4. Guest Networking

The guest should move from DHCP-only startup to explicit boot-arg networking in vmnet mode.

Add darwin guest boot args mirroring Firecracker:

- `cleanroom_guest_ip`
- `cleanroom_guest_gw`
- `cleanroom_guest_mask`
- `cleanroom_guest_dns`

This makes guest identity deterministic and avoids depending on guest DHCP behavior for the new mode.

### 5. Gateway Integration

In current NAT mode, gateway scoping relies on scope-token headers because there is no stable guest IP identity.

In vmnet mode, darwin should move to source-IP sandbox identity similar to Firecracker:

- register sandbox in the gateway by guest IP
- use host IP as the guest-visible gateway address
- keep scope-token flow only for NAT fallback mode

### 6. Sandbox Metadata

Expose vmnet network identity in sandbox state:

- network mode
- host IP
- guest IP
- subnet

This enables future host-to-guest access patterns and debugging.

## Testing Plan

Add opt-in darwin-vz e2e coverage for vmnet mode:

- sandbox gets deterministic guest IP
- host can connect to a guest-bound TCP port
- guest still has outbound internet access
- two sandboxes get distinct subnets
- terminating a sandbox tears down reachability
- gateway registration uses guest IP in vmnet mode

Keep existing NAT-mode tests intact.

## Risks

- macOS version support split between NAT mode and vmnet mode
- additional entitlement/signing requirements
- helper/network cleanup complexity on crash paths
- vmnet mode improves identity but still does not create a literal Linux TAP device
- egress allowlist enforcement remains a separate problem even after guest IP identity exists

## Open Questions

- Is vmnet shared-mode direct host-to-guest TCP reachability sufficient, or do we need explicit port-forward rules for the first host-access slice?
- Do we want one NIC in shared mode, or a later dual-NIC design with one host-only interface and one outbound-NAT interface?
- Should vmnet mode stay darwin-specific in runtime config, or is it worth defining a backend-neutral "host-reachable guest network" capability first?

## Suggested Slices

### Slice 0: Spike

- Prove a single `darwin-vz` VM can boot with vmnet shared mode
- Prove host can reach a guest-bound port
- Prove guest still has outbound internet access

### Slice 1: Experimental Mode

- Add runtime config and helper wiring
- Add per-sandbox subnet planning
- Add boot-arg guest IP configuration
- Persist network metadata

### Slice 2: Gateway Identity

- Switch vmnet mode from scope-token gateway identity to guest-IP identity
- Keep NAT mode unchanged

### Slice 3: Hardening

- doctor checks for entitlement and vmnet support
- teardown and crash recovery
- observability fields for network identity and mode
- CI strategy for supported macOS workers
