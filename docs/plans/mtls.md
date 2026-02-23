# mTLS for cleanroom control plane

## Goal

Add mutual TLS to the cleanroom client/server transport so that both sides authenticate via certificates. This replaces the current `h2c` (cleartext HTTP/2) path for `https://` endpoints.

## Non-goals

- Replacing Tailscale transport (tsnet/tssvc continue to work as-is).
- E2E encryption to the guest agent (tier 2 — future work).
- OIDC or token-based auth (complementary, not blocked by this).
- Certificate rotation or ACME integration.

## Current state

- `https://` scheme is parsed by `internal/endpoint` but the server returns an error: "TLS configuration is not implemented".
- Client `buildTransport` returns a bare `http.Transport{}` for `https` (uses system roots, no client cert).
- No CA, no cert generation, no client cert verification.

## Design

### Certificate layout

All TLS material lives under `$XDG_CONFIG_HOME/cleanroom/tls/` (default `~/.config/cleanroom/tls/`):

```
tls/
  ca.pem          # CA certificate (public)
  ca.key          # CA private key
  server.pem      # Server certificate signed by CA
  server.key      # Server private key
  client.pem      # Client certificate signed by CA
  client.key      # Client private key
```

### CLI commands

#### `cleanroom tls init`

Generates a new CA keypair plus one server and one client certificate. Writes to XDG config dir. Refuses to overwrite existing CA unless `--force` is passed.

- CA: self-signed, ECDSA P-256, 1 year validity, `CN=cleanroom-ca`.
- Server cert: signed by CA, SANs include `localhost` and `127.0.0.1`, 1 year validity.
- Client cert: signed by CA, `CN=cleanroom-client`, 1 year validity.

#### `cleanroom tls issue --name <name> [--san <host>...]`

Issues an additional certificate signed by the existing CA. Useful for issuing certs for specific hosts or additional clients.

### Server changes (`cleanroom serve`)

When `--listen https://...` is used:

1. Auto-discover `server.pem`, `server.key`, `ca.pem` from XDG TLS dir.
2. Build `tls.Config` with server certificate.
3. If `ca.pem` is found, set `ClientAuth: tls.RequireAndVerifyClientCert` with CA pool (mTLS mode).
4. Wrap TCP listener with `tls.NewListener`.
5. Serve with standard `http2` (ALPN negotiation), not `h2c`.

Override flags: `--tls-cert`, `--tls-key`, `--tls-ca`.

### Client changes (`cleanroom exec`, `cleanroom console`, etc.)

When `--host https://...` is used:

1. Auto-discover `client.pem`, `client.key`, `ca.pem` from XDG TLS dir.
2. Build `tls.Config` with client certificate and CA root pool.
3. Use standard `http.Transport{TLSClientConfig: tlsConfig}`.

Override flags: `--tls-cert`, `--tls-key`, `--tls-ca`.

Environment variable fallbacks: `CLEANROOM_TLS_CERT`, `CLEANROOM_TLS_KEY`, `CLEANROOM_TLS_CA`.

### Discovery order

For each of cert, key, and CA:

1. Explicit `--tls-*` flag
2. `CLEANROOM_TLS_*` environment variable
3. XDG config dir auto-discovery

## Implementation slices

### Slice 1: TLS bootstrap package

New package `internal/tlsbootstrap` with:

- `GenerateCA() (certPEM, keyPEM []byte, error)` — ECDSA P-256, self-signed.
- `IssueCert(caCert, caKey, name string, sans []string) (certPEM, keyPEM []byte, error)` — signs a leaf cert.
- `WriteTLSDir(dir string, ca, server, client materials)` — writes PEM files with `0600` permissions on keys.

Tests: generate CA, issue cert, verify cert chains with `x509.Certificate.Verify`.

### Slice 2: `cleanroom tls` CLI commands

- Wire `tls init` subcommand in `internal/cli/cli.go`.
- Wire `tls issue` subcommand.
- Both use `internal/tlsbootstrap`.

### Slice 3: Server TLS listener

- Add `--tls-cert`, `--tls-key`, `--tls-ca` flags to `ServeCommand`.
- Add TLS discovery function: `internal/tlsconfig.Discover(role, flagCert, flagKey, flagCA)`.
- Update `createListener` in `server.go`: when scheme is `https`, build `tls.Config` and wrap listener.
- When TLS is active, serve with `&http2.Server{}` directly (no `h2c` wrapper).

### Slice 4: Client TLS transport

- Add `--tls-cert`, `--tls-key`, `--tls-ca` flags to `ExecCommand` and `ConsoleCommand`.
- Update `controlclient.New` to accept TLS config options.
- Update `buildTransport` for `https` scheme: load client cert and CA, return `http.Transport` with `TLSClientConfig`.

### Slice 5: Integration test

- Start server with `--listen https://127.0.0.1:0` and generated certs.
- Connect client with matching client cert.
- Verify: successful RPC, rejected connection without client cert, rejected connection with wrong CA.

## Files to modify

| File | Change |
|---|---|
| `internal/tlsbootstrap/` (new) | CA + cert generation |
| `internal/tlsconfig/` (new) | TLS config discovery and loading |
| `internal/cli/cli.go` | `tls init`, `tls issue` commands, TLS flags on `serve`/`exec`/`console` |
| `internal/controlserver/server.go` | `https` listener with TLS, drop `h2c` when TLS active |
| `internal/controlclient/client.go` | `buildTransport` TLS path, accept TLS options |

## UX examples

```bash
# One-time setup
cleanroom tls init

# Server (auto-discovers certs)
cleanroom serve --listen https://0.0.0.0:7777

# Client (auto-discovers certs)
cleanroom exec --host https://remote-host:7777 -- npm test

# Explicit cert paths (override)
cleanroom exec --host https://remote-host:7777 \
  --tls-cert /path/to/client.pem \
  --tls-key /path/to/client.key \
  --tls-ca /path/to/ca.pem \
  -- npm test
```
