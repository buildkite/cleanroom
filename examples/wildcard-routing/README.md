# Wildcard Routing Example

Runs an in-guest Python HTTP server behind Cleanroom HTTPS exposure to prove:

- exact and wildcard host exposure can coexist
- guest apps see the original external host and forwarded headers
- a guest-generated redirect can depend on `X-Forwarded-For`

The guest layout is:

- a small Python server listens on `0.0.0.0:80`
- `example.cleanroom.localhost` returns a plain text exact-route response
- `app.example.cleanroom.localhost` returns the original host and forwarded headers
- `s3.example.cleanroom.localhost` returns a redirect derived from forwarded headers

## Prerequisites

From repository root:

```bash
mise run install
```

Start the local control plane if it is not already running:

```bash
mise exec -- cleanroom serve &
```

Install or refresh local DNS and TLS trust for nested `cleanroom.localhost`
hosts:

```bash
sudo cleanroom dns install
```

## Usage

Run from the example directory:

```bash
cd examples/wildcard-routing
mise exec -- cleanroom policy validate
```

Start the example with one exact exposure and one wildcard exposure:

```bash
mise exec -- cleanroom exec \
  --backend darwin-vz \
  --expose-https example:80 \
  --expose-https '*.example:80' \
  -- sh -lc 'cd /workspace/examples/wildcard-routing && sh ./start.sh'
```

While that command is running in another terminal, verify the routes from the
host:

```bash
sh ./verify.sh 8143
```

If Cleanroom chooses a different HTTPS listener port, pass that port to
`verify.sh`.

## What This Exercises

- exact route registration for `example.cleanroom.localhost`
- single-label wildcard route registration for `*.example.cleanroom.localhost`
- guest-side host-based virtual routing
- preserved `Host`, `X-Forwarded-Host`, `X-Forwarded-Proto`,
  `X-Forwarded-Port`, and `X-Forwarded-For`
- a guest-generated redirect from `s3.example.cleanroom.localhost` to
  `app.example.cleanroom.localhost` whose query string is derived from the
  forwarded client chain

## Expected Results

- `https://example.cleanroom.localhost:8143/` returns a plain text response
  from the exact route
- `https://app.example.cleanroom.localhost:8143/` returns JSON showing the
  original host plus forwarded headers
- `https://s3.example.cleanroom.localhost:8143/` returns a redirect to
  `https://app.example.cleanroom.localhost:8143/from-s3?...`

## Notes

- The example uses the Debian agents base image because it already includes
  `python3`, keeping startup deterministic across backends.
- The example keeps all runtime behavior backend-agnostic and does not depend
  on a backend-specific guest network layout.
