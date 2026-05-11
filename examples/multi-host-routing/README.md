# Multi-host Routing Example

Runs an in-guest `nginx` proxy behind Cleanroom HTTPS exposure to prove:

- three exact single-label hostnames can point at the same guest port
- guest apps see the original external host and forwarded headers
- a guest-generated redirect can use forwarded HTTPS metadata

The guest layout is:

- `nginx` listens on `0.0.0.0:80`
- `example.cleanroom.localhost` is served directly by `nginx`
- `example-app.cleanroom.localhost` proxies to a small Python backend on `127.0.0.1:18080`
- `example-s3.cleanroom.localhost` proxies to a small Python backend on `127.0.0.1:18081`

## Prerequisites

From the repository root:

```bash
mise run install
```

Start the local control plane if it is not already running:

```bash
mise exec -- cleanroom serve &
```

Install or refresh local DNS and TLS trust for `cleanroom.localhost` hosts:

```bash
sudo cleanroom dns install
```

## Usage

Run from the example directory:

```bash
cd examples/multi-host-routing
mise exec -- cleanroom policy validate
```

Start the example with three exact HTTPS hosts pointing at the same guest port:

```bash
mise exec -- cleanroom exec \
  --backend darwin-vz \
  --expose-https example:80 \
  --expose-https example-app:80 \
  --expose-https example-s3:80 \
  -- sh -lc 'cd /workspace/examples/multi-host-routing && sh ./start.sh'
```

While that command is running in another terminal, verify the routes from the
host:

```bash
sh ./verify.sh 8143
```

If Cleanroom chooses a different HTTPS listener port, pass that port to
`verify.sh`.

## What This Exercises

- exact route registration for hosts covered by the existing
  `*.cleanroom.localhost` TLS certificate
- guest-side host-based virtual hosting in `nginx`
- preserved `Host`, `X-Forwarded-Host`, `X-Forwarded-Proto`,
  `X-Forwarded-Port`, and `X-Forwarded-For`
- a guest-generated redirect from `example-s3.cleanroom.localhost` to
  `example-app.cleanroom.localhost` whose query string is derived from the
  forwarded client chain

## Expected Results

- `https://example.cleanroom.localhost:8143/` returns a plain text response
  from the exact route
- `https://example-app.cleanroom.localhost:8143/` returns JSON showing the
  original host plus forwarded headers
- `https://example-s3.cleanroom.localhost:8143/` returns a redirect to
  `https://example-app.cleanroom.localhost:8143/from-s3?...`
- unregistered hosts, such as `example-missing.cleanroom.localhost`, return a
  Cleanroom `404 page not found`

## Notes

- The first run is slow because the guest installs `nginx` and `python3`
  through `apt-get`
- The example keeps all runtime behavior backend-agnostic and does not require
  project-specific certificate domains
