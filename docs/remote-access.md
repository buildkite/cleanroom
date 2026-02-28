# Remote access

The Cleanroom server supports multiple listener types for remote access.

## Tailscale

### Embedded tsnet

```bash
cleanroom serve --listen tsnet://cleanroom:7777
cleanroom exec --host http://cleanroom.tailnet.ts.net:7777 -c /path/to/repo -- npm test
```

### Tailscale Service (via local tailscaled)

```bash
cleanroom serve --listen tssvc://cleanroom
cleanroom exec --host https://cleanroom.<your-tailnet>.ts.net -- npm test
```

## HTTP

```bash
cleanroom serve --listen http://0.0.0.0:7777
```

## HTTPS with mTLS

See [TLS and mTLS](tls.md) for certificate setup and auto-discovery.

```bash
cleanroom serve --listen https://0.0.0.0:7777
```
