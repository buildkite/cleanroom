# TLS

Cleanroom supports HTTPS transport with server-auth TLS.

## Serve with HTTPS

```bash
cleanroom serve --listen https://0.0.0.0:7777 \
  --tls-cert /path/to/server.pem \
  --tls-key /path/to/server.key
```

If explicit paths are omitted, cleanroom auto-discovers:

- server cert: `<tlsdir>/server.pem`
- server key: `<tlsdir>/server.key`

`<tlsdir>` defaults to `$XDG_CONFIG_HOME/cleanroom/tls/` (typically `~/.config/cleanroom/tls/`).

Environment variables for server TLS:

- `CLEANROOM_TLS_CERT`
- `CLEANROOM_TLS_KEY`

## Connect over HTTPS

```bash
cleanroom exec --host https://server.example.com:7777 -- echo hello
```

To trust a custom CA, use:

```bash
cleanroom exec --host https://server.example.com:7777 \
  --tls-ca /path/to/ca.pem -- echo hello
```

If `--tls-ca` is omitted, cleanroom uses system roots and falls back to `<tlsdir>/ca.pem` when present.

Environment variable for client CA trust:

- `CLEANROOM_TLS_CA`

## Notes

- Client certificate authentication (mTLS) is not supported.
- Built-in TLS certificate generation commands are not provided.
