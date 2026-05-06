# Sandbox Port Exposure Plan

Built on by docs/plans/sandbox-port-exposure.md

**Status:** Implemented in branch
**Last reviewed:** 2026-05-02

## Summary

Expose sandbox services from the client that requests them, not from the
daemon. This keeps the feature correct when the Cleanroom CLI is connected to a
remote control server: the browser or local client reaches listeners on the
machine that ran `cleanroom exec`, `cleanroom expose`, or
`cleanroom port-forward`, while the control server only brokers byte streams to
the sandbox backend.

```bash
cleanroom exec --expose 5432 -- postgres
cleanroom exec --expose 15432:5432 -- postgres
cleanroom exec --expose-https buildkite:3000 -- npm run dev

cleanroom expose --in <sandbox-id> --expose-https buildkite:3000
cleanroom port-forward --in <sandbox-id> 15432:5432
```

## Design

### Backend Contract

Backends that can reach sandbox guest interfaces implement:

```go
DialSandboxPort(ctx context.Context, sandboxID string, port int) (net.Conn, error)
```

Firecracker dials the known guest IP directly. `darwin-vz` dials through the
file-handle network gateway when that network mode is active. Backends that do
not implement the interface fail closed.

### Control Plane Stream

The control API exposes a bidirectional `DialSandboxPort` stream:

```proto
rpc DialSandboxPort(stream SandboxPortFrame) returns (stream SandboxPortFrame);
```

The first client frame opens a sandbox port. Subsequent frames carry raw bytes
or close the write side. The server validates sandbox readiness and backend
support, dials the backend, sends an open result, then copies bytes between the
stream and backend connection.

### Client Listeners

The CLI owns loopback listeners:

- `--expose <guest-port>` maps `127.0.0.1:<guest-port>` to the sandbox port.
- `--expose <host-port>:<guest-port>` maps an explicit local port to the
  sandbox port.
- `--expose-https [name:]<guest-port>` serves
  `https://<name>.cleanroom.localhost:<port>`.

HTTPS terminates TLS on the client and proxies HTTP/WebSocket traffic over the
control stream. The default hostname label is the DNS-safe sandbox ID; named
aliases are process-local and must be unique while active. The first HTTPS
exposure process uses `127.0.0.1:8143` when available; parallel exposure
processes fall back to another loopback port and print that port in their URLs.

### DNS and Trust

On macOS, `sudo cleanroom dns install` installs
`/etc/resolver/cleanroom.localhost` and trusts the Cleanroom local exposure
certificate. The foreground exposing process runs the DNS server and HTTPS
proxy while exposures are live. DNS answers are wildcarded for the
`cleanroom.localhost` suffix so parallel HTTPS exposure processes can share, and
take over, the resolver listener. `cleanroom dns uninstall` removes the managed
resolver, trust entry, and local certificate material.

## Non-Goals

- Do not store exposures in `cleanroom.yaml`.
- Do not store exposures in sandbox state.
- Do not make `sandbox create --expose` durable.
- Do not bind exposure listeners outside loopback.
- Do not add configurable listener addresses until there is a concrete need.
