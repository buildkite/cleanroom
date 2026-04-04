# Darwin VZ File-Handle Network Backend Plan

## Goal

Evaluate a `VZFileHandleNetworkDeviceAttachment` backend for `darwin-vz` that
can provide:

1. a stable private IP per sandbox
2. outbound internet access with Cleanroom-owned egress filtering
3. future host-to-sandbox addressing by stable name and/or published port

This is a follow-on plan after the `vmnet-shared` filtering experiments.

## Why

The current `vmnet-shared` path gives us guest identity, but not a usable host
filtering point:

- `NEFilterDataProvider` does not see the guest flows
- `NEFilterPacketProvider` sees post-NAT host traffic, not guest-source egress
- `pf` rules against guest source `10.233.0.2` never match packets
- an extra host endpoint created with `vmnet_interface_start_with_network`
  sees gateway-side traffic, not guest transit traffic

So the current problem is not policy compilation. It is packet visibility.

`VZFileHandleNetworkDeviceAttachment` is the first supported virtualization
attachment that gives Cleanroom direct ownership of the packet path.

## What The API Gives Us

Apple documents `VZFileHandleNetworkDeviceAttachment` as:

- a network device attachment that sends raw packets/frames over a file handle
- the file handle must hold a connected datagram socket
- traffic is at the data-link layer

That means:

- no built-in NAT
- no built-in DHCP
- no built-in routing
- no built-in firewalling
- no built-in host-visible guest interface

Those are not missing features. They are the point of the spike: Cleanroom
would own those responsibilities instead of asking the host OS to expose the
right filtering hook.

## Desired End State

### 1. Stable Sandbox IP

Each sandbox gets a private IP on a Cleanroom-owned virtual network, for
example:

- gateway: `10.233.0.1`
- sandbox: `10.233.0.2`

The IP does not need to be globally routable from the host OS. It needs to be a
stable identity within the Cleanroom-owned network backend.

### 2. Internet Egress With Filtering

Guest traffic should leave through a Cleanroom-owned gateway path. That gateway
can enforce policy before opening outbound traffic.

This is the key architectural change:

- filtering moves into Cleanroom's own gateway/backend
- no Network Extension is required for darwin-vz egress filtering

### 3. Future Host Addressing

The likely user-facing shape is not "host routing directly to guest IP."
Instead, it is:

- host-side published ports
- stable internal names such as `sandbox-id.cleanroom.internal`
- optional reverse proxy or port-forward layer on the host

That is sufficient for:

- browser/devserver access
- service-to-service local development
- future "open this sandbox service from the host" UX

## Proposed Architecture

### Helper / VM Side

The darwin-vz helper attaches the guest NIC using
`VZFileHandleNetworkDeviceAttachment`.

The attachment is backed by a connected Unix datagram socket:

- the Swift helper binds a guest-side `unixgram` socket in the sandbox run dir
- the helper connects it to a Go listener socket in the same run dir
- the helper sends the vfkit transport magic (`VFKT`) before VM start
- the connected guest-side file descriptor is passed directly to
  `VZFileHandleNetworkDeviceAttachment`

The guest is configured statically from boot args:

- guest IP
- prefix length
- default gateway
- DNS server

### Host Network Backend

Cleanroom runs a small user-space network backend for each sandbox.

The current spike uses `gvisor-tap-vsock` rather than a custom stack from
scratch:

- Go starts a `gvisor-tap-vsock` virtual network bound to a `unixgram` listener
- the listener accepts the vfkit-style socket connection from the helper
- `gvisor-tap-vsock` provides the virtual gateway, DNS, and outbound transport
  path

That backend is responsible for:

- reading and writing raw Ethernet frames
- ARP / IPv4 neighbor behavior
- basic L3 routing decisions
- outbound connection handling
- policy enforcement
- optional host port publication

### Egress Model

The simplest useful first version is not full transparent NAT. It is a
Cleanroom-owned egress gateway with a narrow scope:

- IPv4 only
- TCP first
- DNS explicitly handled

That is enough to prove:

- per-sandbox identity
- deny-by-default egress
- future hostname policy support

## Spike Order

### Phase 1: Frame Visibility

Success criteria:

- one VM boots on `VZFileHandleNetworkDeviceAttachment`
- host backend receives raw frames from the guest
- host backend can identify guest MAC and IPv4 source

Implementation:

- attach VM NIC to a connected `unixgram` socket
- accept the connection from Go via `gvisor-tap-vsock`
- verify guest-originated Ethernet traffic reaches the Go backend

Status:

- done
- guest boots with a static `10.233.0.2/24`
- Go sees the guest at layer 2 through the file-handle attachment
- no Network Extension or vmnet host filtering is involved

### Phase 2: Private L3 Network

Success criteria:

- guest uses a static IP such as `10.233.0.2/24`
- host backend acts as gateway `10.233.0.1`
- guest can send ARP and IPv4 packets to the gateway

Implementation:

- respond to ARP for the gateway IP
- accept guest traffic destined to the gateway
- do not attempt general internet egress yet

This phase proves stable sandbox identity.

### Phase 3: Minimal Internet Egress

Success criteria:

- guest can reach a public host through the backend
- backend sees guest IP before opening outbound traffic

Implementation choices, in order of simplicity:

1. reuse `gvisor-tap-vsock` for gateway, DNS, and outbound transport
2. prove one outbound HTTPS fetch through the backend
3. only then add Cleanroom policy checks before outbound connect

Status:

- partially done
- guest egress to the public internet is working through the Go gateway
- filtering is not implemented yet

### Phase 4: Policy Enforcement

Success criteria:

- deny-by-default blocks non-allowed egress
- allow rules are enforced by the backend before opening outbound traffic

Implementation:

- policy keyed by sandbox identity, not host PID
- initial matching can be:
  - guest IP
  - protocol
  - remote IP / port
- hostname policy can be compiled by the gateway/backend

### Phase 5: Host Reachability

Success criteria:

- the host can reach a guest service through a stable local name or published
  port

Implementation:

- add host-side port publishing or reverse proxy
- optional internal DNS name such as:
  - `<sandbox-id>.cleanroom.internal`

This is a product feature layer on top of the owned network backend, not a
prerequisite for filtering.

## Non-Goals For The Spike

- no IPv6 in the first slice
- no full generic L2 switch behavior
- no multi-sandbox shared network in the first slice
- no Network Extension integration for darwin-vz
- no attempt to preserve the current vmnet filtering path

## Recommended First Spike

Implement the smallest end-to-end path:

1. new experimental `darwin-vz` network mode backed by
   `VZFileHandleNetworkDeviceAttachment`
2. static guest IP `10.233.0.2`
3. host gateway identity `10.233.0.1`
4. host backend logs Ethernet / ARP / IPv4 frames
5. prove guest traffic is visible before any translation occurs

If that phase fails, stop. The approach is not worth carrying further.

If that phase succeeds, move directly to TCP-only outbound relay and policy
checks.

## Expected Product Consequence

If this backend works, the darwin-vz egress filtering story becomes:

- Cleanroom-owned guest networking
- Cleanroom-owned policy enforcement
- no dependency on macOS Network Extension for darwin-vz filtering

That would simplify the product boundary substantially compared with the
current support-app + system-extension path.

## Sources

- [VZFileHandleNetworkDeviceAttachment.h](/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.0.sdk/System/Library/Frameworks/Virtualization.framework/Versions/A/Headers/VZFileHandleNetworkDeviceAttachment.h)
- [VZNetworkDeviceAttachment.h](/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.0.sdk/System/Library/Frameworks/Virtualization.framework/Versions/A/Headers/VZNetworkDeviceAttachment.h)
- [docs/plans/darwin-vz-vmnet-mode.md](/Users/lachlan/Develop/lox/cleanroom/docs/plans/darwin-vz-vmnet-mode.md)
- [docs/plans/darwin-vz-network-filter-packet-mode.md](/Users/lachlan/Develop/lox/cleanroom/docs/plans/darwin-vz-network-filter-packet-mode.md)
