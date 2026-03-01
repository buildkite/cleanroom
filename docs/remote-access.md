# Remote access

The Cleanroom server supports HTTP and HTTPS listener modes for remote access.

## HTTP

```bash
cleanroom serve --listen http://0.0.0.0:7777
```

## HTTPS

See [TLS](tls.md) for certificate setup.

```bash
cleanroom serve --listen https://0.0.0.0:7777 \
  --tls-cert /path/to/server.pem \
  --tls-key /path/to/server.key
```
