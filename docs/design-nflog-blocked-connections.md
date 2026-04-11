# Design: NFLOG-based Blocked Connection Detection (Firecracker)

## Problem

PR #167 added DNS-layer blocked connection detection via `dnsproxy.Forwarder.OnDeny`. When a guest resolves a hostname not in the network allow policy, a warning streams to the client immediately. This covers ~99% of real traffic — but literal IP connections (those bypassing DNS entirely) hit the terminal `iptables -A FORWARD -i <tap> -j DROP` silently with no user-visible feedback.

The darwin-vz backend detects these at TCP connect time in the gvisor forwarder (`filehandle_gateway.go:433-449`). This design brings firecracker to parity using Linux NFLOG.

## Approach

Insert an `iptables -j NFLOG` rule immediately before the terminal DROP in the per-sandbox FORWARD chain. A userspace Go listener on the NFLOG netlink socket receives dropped packet metadata, parses the IP header for dest IP/port/protocol, reverse-looks up via `dnsproxy.Runtime.NamesForAddress()`, and emits a warning via `backend.WarningEmitter`.

## Architecture

```
Guest VM → TAP → FORWARD chain
  → crdns-tcp chain (ACCEPT for DNS-observed hosts)
  → crdns-udp chain (ACCEPT for DNS-observed hosts)
  → literal IP ACCEPT rules
  → NFLOG --nflog-group <N>  ← NEW: captures about-to-be-dropped packets
  → DROP (terminal)
                                    ↓
                        NFLOG netlink socket (userspace)
                                    ↓
                        Parse IPv4 header → (destIP, destPort, protocol)
                                    ↓
                        dnsproxy.Runtime.NamesForAddress() → hostname enrichment
                                    ↓
                        backend.WarningEmitter.Emit("network connection blocked: host:port")
```

## Detailed Design

### 1. NFLOG Group Allocation

Each sandbox needs a unique NFLOG group (uint16, 0-65535). Derive it deterministically from the TAP name to avoid collisions between concurrent sandboxes:

```go
func nflogGroupFromTapName(tapName string) uint16 {
    h := fnv.New32a()
    h.Write([]byte(tapName))
    // Range 100-65535 to avoid collisions with system groups (0-99)
    return uint16(100 + (h.Sum32() % (65536 - 100)))
}
```

The group is scoped per-sandbox and cleaned up with the iptables rule on sandbox termination.

### 2. iptables Rule Changes

In `setupHostNetworkWithTrustedDNSFactory`, insert the NFLOG rule immediately before the terminal DROP (only in deny-default mode):

```go
// Current (line ~2361):
if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-j", "DROP"); err != nil { ... }

// New:
nflogGroup := nflogGroupFromTapName(tapName)
if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName,
    "-j", "NFLOG", "--nflog-group", strconv.Itoa(int(nflogGroup))); err != nil { ... }
addCleanup("iptables", "-D", "FORWARD", "-i", tapName,
    "-j", "NFLOG", "--nflog-group", strconv.Itoa(int(nflogGroup)))
// Then the existing DROP:
if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-j", "DROP"); err != nil { ... }
```

### 3. Root Helper Changes

**New capability**: `firecracker-nflog`

Add to `helper_capabilities()`:
```bash
echo "firecracker-nflog"
```

Add NFLOG rule validation to `run_iptables()`:
```bash
# NFLOG: iptables -A|-D FORWARD -i <tap> -j NFLOG --nflog-group <group>
if [[ "$#" -eq 8 && ( "$1" == "-A" || "$1" == "-D" ) && "$2" == "FORWARD" && "$3" == "-i" && "$5" == "-j" && "$6" == "NFLOG" && "$7" == "--nflog-group" ]]; then
    is_tap_name "$4" || die "iptables NFLOG: unsupported interface '$4'"
    is_numeric "$8" || die "iptables NFLOG: invalid group '$8'"
    exec /usr/sbin/iptables "$@"
fi
```

Bump `helper_contract_version()` to `"6"`.

### 4. NFLOG Listener

New file: `internal/backend/firecracker/nflog_listener.go` (linux-only build tag).

Library: `github.com/florianl/go-nflog/v2` — pure Go, C-binding free, MIT licensed, well maintained. It requires `CAP_NET_ADMIN` which the cleanroom process already has (it opens netlink sockets for iptables via the root helper; the NFLOG netlink socket is opened by the cleanroom process itself which already runs with appropriate capabilities).

**Key concern**: `CAP_NET_ADMIN` is needed to open the NFLOG netlink socket. The cleanroom process may not have this capability. Two options:
- **(a)** If the process runs as root or has `CAP_NET_ADMIN`: open the socket directly.
- **(b)** If not: use `CopyMeta` mode which only delivers packet metadata (IP header), not full payload. Still needs `CAP_NET_ADMIN`. If the process lacks it, NFLOG gracefully degrades to no-op (log a warning during preflight, skip NFLOG setup).

```go
//go:build linux

package firecracker

import (
    "context"
    "encoding/binary"
    "log"
    "net/netip"
    "sync"
    "time"

    nflog "github.com/florianl/go-nflog/v2"
    "github.com/buildkite/cleanroom/internal/backend"
    "github.com/buildkite/cleanroom/internal/dnsproxy"
    "github.com/mdlayher/netlink"
)

type nflogListenerConfig struct {
    group     uint16
    sandboxID string
    guestIP   netip.Addr
    runtime   *dnsproxy.Runtime
    warnings  *backend.WarningEmitter
}

type nflogListener struct {
    nf     *nflog.Nflog
    cancel context.CancelFunc
    done   chan struct{}
}

func newNFLogListener(cfg nflogListenerConfig) (*nflogListener, error) {
    nf, err := nflog.Open(&nflog.Config{
        Group:    cfg.group,
        Copymode: nflog.CopyPacket,
        Bufsize:  128, // Only need IP + TCP/UDP headers (≤40 bytes)
    })
    if err != nil {
        return nil, fmt.Errorf("open nflog group %d: %w", cfg.group, err)
    }

    if err := nf.Con.SetReadBuffer(64 * 1024); err != nil {
        _ = nf.Close()
        return nil, fmt.Errorf("set nflog read buffer: %w", err)
    }
    if err := nf.SetOption(netlink.NoENOBUFS, true); err != nil {
        _ = nf.Close()
        return nil, fmt.Errorf("set nflog NoENOBUFS: %w", err)
    }

    ctx, cancel := context.WithCancel(context.Background())
    listener := &nflogListener{
        nf:     nf,
        cancel: cancel,
        done:   make(chan struct{}),
    }

    hook := func(attrs nflog.Attribute) int {
        if attrs.Payload == nil {
            return 0
        }
        destIP, destPort, proto, ok := parseIPv4PacketHeader(*attrs.Payload)
        if !ok {
            return 0
        }
        dest := destIP.String()
        if cfg.runtime != nil {
            if names := cfg.runtime.NamesForAddress(cfg.sandboxID, cfg.guestIP, destIP, time.Now()); len(names) > 0 {
                dest = strings.Join(names, ",")
            }
        }
        msg := fmt.Sprintf("network connection blocked: %s:%d", dest, destPort)
        cfg.warnings.Emit(msg)
        return 0
    }

    errFn := func(err error) int {
        if ctx.Err() != nil {
            return 1 // shutting down
        }
        log.Printf("nflog listener error for sandbox %s: %v", cfg.sandboxID, err)
        return 0
    }

    if err := nf.RegisterWithErrorFunc(ctx, hook, errFn); err != nil {
        cancel()
        _ = nf.Close()
        return nil, fmt.Errorf("register nflog hook: %w", err)
    }

    go func() {
        defer close(listener.done)
        <-ctx.Done()
    }()

    return listener, nil
}

func (l *nflogListener) Close() error {
    l.cancel()
    <-l.done
    return l.nf.Close()
}

// parseIPv4PacketHeader extracts dest IP, dest port, and protocol from a raw
// IPv4 packet. Returns false if the packet is too short or not IPv4/TCP/UDP.
func parseIPv4PacketHeader(payload []byte) (destIP netip.Addr, destPort uint16, protocol string, ok bool) {
    if len(payload) < 20 {
        return netip.Addr{}, 0, "", false
    }
    version := payload[0] >> 4
    if version != 4 {
        return netip.Addr{}, 0, "", false
    }
    ihl := int(payload[0]&0x0f) * 4
    if ihl < 20 || len(payload) < ihl+4 {
        return netip.Addr{}, 0, "", false
    }

    proto := payload[9]
    var protoStr string
    switch proto {
    case 6: // TCP
        protoStr = "tcp"
    case 17: // UDP
        protoStr = "udp"
    default:
        return netip.Addr{}, 0, "", false
    }

    addr, ok := netip.AddrFromSlice(payload[16:20])
    if !ok {
        return netip.Addr{}, 0, "", false
    }
    port := binary.BigEndian.Uint16(payload[ihl+2 : ihl+4])

    return addr, port, protoStr, true
}
```

### 5. Lifecycle Integration

The NFLOG listener slots into `setupHostNetworkWithTrustedDNSFactory` alongside the trusted DNS service. It follows the same cleanup pattern:

```go
// In setupHostNetworkWithTrustedDNSFactory, after the NFLOG iptables rule:
nflogCleanup := func() {}
if !allowAll {
    nflogListener, err := newNFLogListener(nflogListenerConfig{
        group:     nflogGroup,
        sandboxID: runID,
        guestIP:   guestAddr,
        runtime:   trustedDNSRuntime, // from the trusted DNS service
        warnings:  warningsPtr,       // atomic pointer, same as DNS deny
    })
    if err != nil {
        log.Printf("nflog listener unavailable for %s: %v", runID, err)
        // Non-fatal: DNS-layer detection still works
    } else {
        nflogCleanup = func() { _ = nflogListener.Close() }
    }
}

// In cleanup:
cleanup := func() {
    nflogCleanup()
    trustedDNSCleanup()
    // ... existing cleanup
}
```

**Important**: The NFLOG listener needs access to the trusted DNS runtime to call `NamesForAddress()`. Currently the runtime is created inside `newTrustedDNSService` and not exposed. Two options:

- **(a) Expose runtime from trusted DNS service**: Return it from the factory alongside cleanup. This changes the `trustedDNSFactory` signature.
- **(b) Create runtime externally**: Create the `dnsproxy.Runtime` in `setupHostNetworkWithTrustedDNSFactory` and pass it to both the trusted DNS service and NFLOG listener. This is cleaner.

**Recommendation**: Option (b). Create the runtime and register the sandbox at the network setup level, then pass the runtime to both `newTrustedDNSService` and `newNFLogListener`.

### 6. Warning Emission

The NFLOG listener uses the same `backend.WarningEmitter` as the DNS deny callback. This gives automatic deduplication — if both DNS and NFLOG fire for the same destination (which shouldn't normally happen but could with races), only one warning reaches the client.

Message format matches darwin-vz: `"network connection blocked: <host-or-ip>:<port>"`

### 7. Rate Limiting

NFLOG can deliver packets at wire speed. Rate limiting is needed at two levels:

1. **iptables level**: Use `--nflog-threshold 1` (default) and consider adding `-m limit --limit 10/sec --limit-burst 20` to the NFLOG rule itself. This keeps kernel→userspace traffic bounded.

2. **Userspace level**: `backend.WarningEmitter` already deduplicates by message string, so repeated connections to the same blocked `host:port` only emit once per handler lifetime. No additional rate limiting needed for the same destination. Different destinations each emit once.

The iptables-level rate limit is the safer approach:
```
iptables -A FORWARD -i <tap> -m limit --limit 10/sec --limit-burst 20 -j NFLOG --nflog-group <N>
```

However, this adds `-m limit` complexity to the root helper validation. An alternative is to accept all packets but rely on WarningEmitter deduplication. Since each unique `host:port` only emits once, and the number of unique blocked destinations in a typical sandbox run is small, this should be fine without iptables-level rate limiting. We can add it later if needed.

### 8. Preflight Check

Add an NFLOG check to `Preflight()`:

```go
if _, err := nflog.Open(&nflog.Config{Group: 65535, Copymode: nflog.CopyMeta}); err != nil {
    appendCheck("nflog", "warn", fmt.Sprintf("nflog unavailable: %v (blocked connection warnings may not work for literal IPs)", err))
} else {
    appendCheck("nflog", "pass", "nflog netlink socket accessible")
}
```

Also check for the new `firecracker-nflog` capability in the root helper.

### 9. Build Constraints

The NFLOG listener is linux-only:
- `nflog_listener.go`: `//go:build linux`
- `nflog_listener_stub.go`: `//go:build !linux` — provides a no-op `newNFLogListener` that returns `nil, nil`.

This matches the existing pattern where the firecracker backend is linux-only but compiles (with stubs) on other platforms.

## Testing Strategy

1. **Unit test `parseIPv4PacketHeader`**: Construct raw IPv4+TCP and IPv4+UDP packets, verify extracted dest IP, port, protocol.

2. **Integration test lifecycle**: Mock the NFLOG open (inject a factory function like `trustedDNSFactory`) so tests can verify the listener starts/stops correctly without requiring `CAP_NET_ADMIN`.

3. **Network test**: Extend `TestSetupHostNetworkWithTrustedDNSFactory*` to verify the NFLOG iptables rule appears before the DROP rule.

4. **Root helper test**: Test the new iptables NFLOG validation in `cleanroom-root-helper.sh` (e.g., `is_nflog_group()` validator).

## Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| `CAP_NET_ADMIN` not available | Graceful degradation: log warning, skip NFLOG. DNS-layer detection still works. |
| NFLOG group collision between sandboxes | FNV hash of tap name across 65435 groups. Collisions are theoretically possible but extremely unlikely with typical sandbox counts. |
| High packet rate overwhelms userspace | WarningEmitter deduplication means each unique `host:port` emits once. Add iptables `-m limit` later if needed. |
| Kernel lacks NFLOG support | Very unlikely on any modern kernel. Detected at `nflog.Open()` time, non-fatal. |
| Root helper not updated | New capability check in preflight. Older helpers reject the NFLOG iptables rule, non-fatal. |

## Implementation Order

1. Root helper: add `firecracker-nflog` capability and NFLOG iptables validation. Bump contract version.
2. `nflog_listener.go` + `nflog_listener_stub.go`: NFLOG listener with packet parsing.
3. Wire into `setupHostNetworkWithTrustedDNSFactory`: iptables rule + listener lifecycle.
4. Preflight check.
5. Tests.

## Dependencies

- `github.com/florianl/go-nflog/v2` (MIT, pure Go, no CGo)
- Transitive: `github.com/mdlayher/netlink` (already used indirectly via other deps)
