# TLS and mTLS

Cleanroom supports mutual TLS for HTTPS transport. TLS material is stored in
`$XDG_CONFIG_HOME/cleanroom/tls/` (typically `~/.config/cleanroom/tls/`).

## Bootstrap certificates

```bash
cleanroom tls init
```

This generates a CA, server certificate (with localhost + hostname SANs), and
client certificate. Use `--force` to overwrite existing material.

## Issue additional certificates

```bash
cleanroom tls issue worker-1 --san worker-1.internal --san 10.0.0.5
```

When `--san` is omitted, the certificate name is added as a SAN automatically.

## Serve with HTTPS + mTLS

```bash
cleanroom serve --listen https://0.0.0.0:7777
```

TLS material is auto-discovered from the XDG TLS directory. To use explicit
paths:

```bash
cleanroom serve --listen https://0.0.0.0:7777 \
  --tls-cert /path/to/server.pem \
  --tls-key /path/to/server.key \
  --tls-ca /path/to/ca.pem
```

When a CA is configured, the server requires and verifies client certificates
(mTLS). Without `--tls-ca`, the server accepts any TLS client.

## Connect over HTTPS

```bash
cleanroom exec --host https://server.example.com:7777 -- echo hello
```

Client certificates and CA are auto-discovered from the XDG TLS directory, or
specified with `--tls-cert`, `--tls-key`, and `--tls-ca`.

Environment variables `CLEANROOM_TLS_CERT`, `CLEANROOM_TLS_KEY`, and
`CLEANROOM_TLS_CA` are also supported.

## Auto-discovery

When no explicit TLS flags are provided, cleanroom looks for:

| Role   | Cert                    | Key                     | CA               |
|--------|-------------------------|-------------------------|------------------|
| Server | `<tlsdir>/server.pem`   | `<tlsdir>/server.key`   | `<tlsdir>/ca.pem` |
| Client | `<tlsdir>/client.pem`   | `<tlsdir>/client.key`   | `<tlsdir>/ca.pem` |

CA auto-discovery is skipped when cert/key are explicitly provided, to avoid
unexpectedly enabling mTLS.
