# Darwin VZ Vmnet Mode Plan

## Goal

Make vmnet-backed networking the default path for `darwin-vz` on supported
macOS 26+ hosts without over-committing to Linux-style TAP semantics.

Near-term target:

- make `vmnet-shared` the default on supported macOS 26+ hosts
- keep explicit `nat` only as a fallback
- ship the minimum helper packaging and signing support needed to satisfy Apple
  VMNet requirements
- prove reliable end-to-end paths for:
  - default shared network boot + egress
  - custom RFC1918 shared subnet boot + egress

This document is a planning document informed by the March 2026 spike. It
includes verified findings, current limits, and recommended next slices.

## Current Status

Validated on March 16, 2026 on macOS 26.1 (25B78):

- `vmnet-shared` can work with `Virtualization.framework` when the helper
  creates the vmnet logical network and attaches it in the same process
- the helper must be a signed `.app` bundle with an embedded macOS
  provisioning profile for `com.buildkite.cleanroom.darwin-vz`
- the working VMNet entitlement/profile key is
  `com.apple.developer.networking.vmnet`
- the default shared network path works: VM boots, guest gets a static IPv4
  derived from the vmnet subnet, and guest has outbound egress
- the custom RFC1918 subnet path also works for `10.233.0.0/16` when the helper
  disables vmnet DHCP, passes the host/gateway address to
  `vmnet_network_configuration_set_ipv4_subnet`, and configures the guest
  statically from boot args

Still not proven:

- deterministic identity across restarts

Also validated:

- host-to-guest TCP reachability works on the custom `10.233.0.0/16` path when
  the guest runs an injected one-shot TCP test binary
- helper-reported vmnet metadata can be persisted into darwin-vz config and run
  observability output for later inspection

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

Vmnet mode is meant to close some of the identity and reachability gap, not to
claim exact TAP parity with Linux.

## Decision

Proceed with vmnet in narrow increments:

1. Treat default `vmnet-shared` as the only known-good first slice.
2. Treat custom RFC1918 shared subnets as experimental but viable when routed
   through the helper-owned static vmnet path.
3. Treat direct host-to-guest TCP reachability as proven on the custom shared
   subnet path, but keep the automation fixture narrow and test-specific.
4. Defer deterministic guest identity, gateway integration, and metadata until
   host reachability and persistence behavior are proven stable enough to build
   on.

## Non-Goals

- no claim of exact Linux TAP or iptables parity
- no LAN bridging in the first slice
- no port publishing API in the first slice
- no gateway or policy plumbing changes in the first slice

## Verified Constraints

- `VZVmnetNetworkDeviceAttachment` is available only on macOS 26+
- a VM can only use a vmnet logical network that was created in the same
  application process that attaches it to the VM
- vmnet-backed networking requires authorization beyond
  `com.apple.security.virtualization`
- on current Apple-managed provisioning, the practical entitlement to satisfy is
  `com.apple.developer.networking.vmnet`

Operational implications:

- the Swift helper must own vmnet network creation
- Go can choose mode and optional subnet, but it should not attempt to create
  vmnet objects
- the helper signing path needs to support a bundled app with an embedded
  profile, not just a loose binary

## Recommended Architecture

### 1. Runtime Config

Keep the runtime surface small:

- `backends.darwin-vz.network.mode`
- optional `backends.darwin-vz.network.subnet`

Defaults:

- `vmnet-shared` becomes the default on supported macOS 26+ hosts
- explicit `nat` remains available as a fallback
- custom subnet support stays explicitly experimental until Apple shared-mode
  subnet behavior is reliable

Do not add subnet pools, DNS overrides, or gateway-facing config in this slice.

### 2. Helper Packaging and Signing

Use the dedicated helper App ID:

- `com.buildkite.cleanroom.darwin-vz`

Required local setup:

1. Enable Apple `VMNet` for `com.buildkite.cleanroom.darwin-vz`.
2. Regenerate and install the matching macOS provisioning profile.
3. Build the helper as a signed `.app` bundle with the embedded profile.

Known-good local build command:

```bash
CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS=cmd/cleanroom-darwin-vz/entitlements-vmnet.plist \
CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY='Apple Development: <you> (<team>)' \
CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER='com.buildkite.cleanroom.darwin-vz' \
CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE="$HOME/Downloads/Cleanroom_Darwin_VZ_Backend.provisionprofile" \
scripts/build-darwin-vz-helper.sh
```

The helper should carry:

- `com.apple.security.virtualization`
- `com.apple.developer.networking.vmnet`

### 3. Helper Networking

In `vmnet-shared` mode, the helper should:

- create a vmnet shared-mode logical network
- disable vmnet DHCP
- optionally apply a custom IPv4 subnet only when explicitly requested
- when applying a custom subnet, pass the host/gateway address rather than the
  raw network address to `vmnet_network_configuration_set_ipv4_subnet`
- create the network object
- query vmnet for the actual subnet after creation
- attach the NIC using `VZVmnetNetworkDeviceAttachment`

The current NAT attachment remains the fallback path.

Recommended behavior:

- use the default shared network when no subnet is configured
- derive the guest IP as the first usable address in the vmnet subnet
- pass guest IPv4, gateway IPv4, and prefix length to the guest via boot args

### 4. Guest Networking

For `vmnet-shared`, prefer static guest networking driven by helper-provided
boot args.

Reason:

- this matches Apple’s `containerization` implementation strategy
- it removes guest dependence on vmnet DHCP
- it works for both the default shared network and explicit RFC1918 subnets on
  the validated host

Guest init should:

- bring the primary NIC up
- apply the helper-provided IPv4/prefix
- install the default route through the helper-provided gateway
- write `/etc/resolv.conf` with the gateway as the nameserver
- fall back to DHCP only when the vmnet static boot args are absent

### 5. Identity and Host Reachability

Do not assume the initial vmnet mode provides a Firecracker-equivalent guest IP
identity story.

Possible future directions:

- direct host-to-guest reachability using the statically assigned guest IP
- longer-lived shared-network allocation if Cleanroom wants Apple-style multiple
  guests on one vmnet network
- vmnet port forwarding for explicit host ingress

Those should be follow-on slices, not part of the first working vmnet mode.

## Testing Plan

### Automated Path

Keep one focused opt-in e2e with two subtests:

- `vmnet-shared` on the default shared network with helper-driven static guest
  config
- `vmnet-shared` on a custom RFC1918 subnet, currently `10.233.0.0/16`
- guest has working outbound egress in both cases
- host can reach a guest TCP listener on the custom subnet path

The reachability subtest should use an injected one-shot Linux guest test
binary, not packages expected to exist in the base image.

Run it with a signed helper bundle:

```bash
CLEANROOM_DARWIN_VZ_HELPER="$PWD/dist/$(go env GOOS)-$(go env GOARCH)/libexec/cleanroom/cleanroom-darwin-vz.app" \
CLEANROOM_DARWIN_VZ_VMNET_E2E=1 \
mise exec -- go test ./internal/backend/darwinvz -run TestVMNetSharedE2E -v
```

### Manual Follow-Up Checks

Keep these as manual or follow-up spike items until the automated path is
stable:

- deterministic guest identity across restarts

## Risks

- macOS version split between `nat` and `vmnet-shared`
- Apple-managed capability and provisioning complexity
- helper crash cleanup for vmnet logical networks
- vmnet mode improves network behavior but still does not create a Linux TAP
- egress allowlist enforcement remains separate from vmnet support
- custom shared subnets may remain OS-version-sensitive even if the API exists

## Open Questions

- Is direct host-to-guest TCP reachable on default shared mode reliable enough,
  or should the first ingress slice use explicit vmnet port forwarding?
- If Cleanroom eventually wants multiple simultaneous guests on one vmnet
  network, should it copy Apple’s longer-lived allocator model instead of
  creating one vmnet logical network per sandbox?
- Do we need helper-reported vmnet metadata persisted in sandbox state, or is
  the current boot-arg handoff enough for the next slice?

## Suggested Slices

### Slice 0: Spike

Status: complete.

What it proved:

- helper packaging and signing can satisfy VMNet on macOS
- default `vmnet-shared` works for boot, static guest config, and egress
- custom `10.233.0.0/16` `vmnet-shared` also works when the helper uses the
  gateway address for subnet configuration and disables DHCP
- host-to-guest TCP reachability works on the custom shared subnet path using
  an injected one-shot guest test binary

What it did not prove:

- guest metadata persistence

### Slice 1: Experimental Mode

- keep the current minimal runtime config and helper wiring
- ship `vmnet-shared` as the default darwin-vz mode with experimental RFC1918
  subnet override support
- keep explicit `nat` as an escape hatch
- keep the focused e2e on the default and custom shared paths

Exit criteria:

- helper signing path is documented and repeatable
- tagged release packaging installs a pre-signed vmnet-capable helper bundle
- doctor surfaces missing vmnet entitlement/profile issues clearly
- default shared-mode e2e passes on supported macOS 26+ hosts
- custom shared-subnet e2e passes on supported macOS 26+ hosts
- host-to-guest reachability e2e passes on supported macOS 26+ hosts

### Slice 2: Reachability Investigation

- decide whether product ingress should rely on direct shared-mode routing or
  vmnet port-forward rules
- decide whether the current injected test binary is sufficient, or whether a
  richer guest fixture is needed for future networking tests

Exit criteria:

- one product-ready host-to-guest reachability story
- one automated or repeatable verification path

### Slice 3: Deterministic Identity

- evaluate deterministic MAC allocation
- decide whether a longer-lived shared-network allocator is preferable to
  helper-per-sandbox vmnet networks
- build on the now-persisted network metadata with a restart-stable identity
  model

Exit criteria:

- stable guest identity without depending on unsupported subnet behavior

## References

Internal:

- [Darwin VZ Backend](../backend/darwin-vz.md)

Apple:

- [apple/containerization](https://github.com/apple/containerization)
- [Supported capabilities (macOS)](https://developer.apple.com/help/account/reference/supported-capabilities-macos/)
- [Provisioning with capabilities](https://developer.apple.com/help/account/reference/provisioning-with-managed-capabilities/)
- [Diagnosing issues with entitlements](https://developer.apple.com/documentation/bundleresources/diagnosing-issues-with-entitlements)
- [Technical Note TN2415: Entitlements Troubleshooting](https://developer.apple.com/library/archive/technotes/tn2415/_index.html)
- [VZVmnetNetworkDeviceAttachment](https://developer.apple.com/documentation/virtualization/vzvmnetnetworkdeviceattachment)
- [vmnet_network_configuration_set_ipv4_subnet](https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_set_ipv4_subnet)

Apple SDK references used in the spike:

- Xcode 26.0 SDK `VZVmnetNetworkDeviceAttachment.h`
- Xcode 26.0 SDK `vmnet.h`
