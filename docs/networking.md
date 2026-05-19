# Networking

Cleanroom policy starts from deny-by-default egress. A sandbox can only connect
to destinations allowed by the active policy, and hostname rules are enforced
through DNS resolution plus destination IP:port checks.

## Egress Policy

```yaml
sandbox:
  network:
    default: deny
    allow:
      - github.com:443
      - host: registry.npmjs.org
        ports: [443]
```

Rules are host and port rules. Do not use URLs such as
`https://github.com`. See [Policy](policy.md) for stage-scoped network blocks.

Known limits:

- Hostname rules are enforced from observed DNS answers and destination
  IP:port pairs, so they do not distinguish co-hosted services sharing the same
  IP and port.
- General UDP and IPv6 allowlist policy is not at parity with the TCP gateway
  path.
- `firecracker` currently rejects stage-scoped network policies. `darwin-vz`
  supports them.

## Host Gateway

Allowed Git, OCI, Docker Hub, Go module, RubyGems, and immutable download
traffic can use the Cleanroom host gateway. The gateway keeps upstream
credentials on the host side and uses
[`content-cache`](https://github.com/buildkite/content-cache) for cache-backed
routes.

The gateway is exposed inside sandboxes as:

```text
http://gateway.cleanroom.internal:8170
```

See [Caching](caching.md) for the user-facing cache model and
[Host Gateway](gateway.md) for route details.

## Exposing Sandbox Services

Use raw TCP when a host client needs to connect to a sandbox port:

```bash
cleanroom exec --expose 15432:5432 -- postgres
```

That maps host `127.0.0.1:15432` to guest port `5432` while the command is
running.

Use local HTTPS when a browser or callback flow needs a stable local hostname:

```bash
cleanroom exec --expose-https buildkite:3000 -- npm run dev
```

The URL is usually:

```text
https://buildkite.cleanroom.localhost:8143
```

For projects that need several hostnames or wildcard labels, declare the routes
in `cleanroom.yaml` and pass `--expose-https` without a value:

```yaml
expose:
  https:
    base: "{sandbox_id}.cleanroom.localhost"
    routes:
      - port: 3000
        hosts:
          - "{base}"
          - "*.{base}"
          - "*.*.{base}"
```

```bash
cleanroom exec --expose-https -- npm run dev
```

`cleanroom dns install` manages `cleanroom.localhost`. Routes under another
local suffix, such as `{sandbox_id}.localhost`, also work when that suffix
resolves to the Cleanroom DNS listener.

For an existing sandbox, use:

```bash
cleanroom expose --in 01kr7p9ksmfa7rmyypyqw2w8r0 --expose 15432:5432
cleanroom expose --in 01kr7p9ksmfa7rmyypyqw2w8r0 --expose-https app:3000
```

## Local DNS And HTTPS Trust

The installer starts the daemon. Local DNS and HTTPS trust are separate because
they change host resolver and trust-store state.

On macOS:

```bash
sudo cleanroom dns install
cleanroom dns status
```

Use `cleanroom dns uninstall` to remove Cleanroom-managed DNS and certificate
trust material.

## Remote Control API

The default client/server transport is a Unix socket. HTTP and HTTPS listeners
are available for remote control-plane access:

```bash
cleanroom serve --listen http://0.0.0.0:7777
cleanroom serve --listen https://0.0.0.0:7777 \
  --tls-cert /path/to/server.pem \
  --tls-key /path/to/server.key
```

Clients can connect with:

```bash
cleanroom exec --host https://server.example.com:7777 \
  --tls-ca /path/to/ca.pem -- echo hello
```

See [Operations](operations.md) for daemon and config commands.
