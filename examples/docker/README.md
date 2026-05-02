# Docker-In-Guest Example

This example boots a sandbox from the Docker image, enables the explicit Docker service contract in policy, and runs `docker` commands inside the sandbox.

## Prerequisites

From repository root:

```bash
mise run install
```

## Files

- `cleanroom.yaml`: digest-pinned Docker image ref, `sandbox.docker.required: true`, and a deny-by-default network allowlist for the upstream Docker Hub endpoints that the host gateway validates and fetches against.

## Quick test flow

Run from this directory (`examples/docker`):

```bash
mise exec -- cleanroom policy validate
```

Start a local control-plane server:

```bash
mise exec -- cleanroom serve &
```

Confirm daemon + client are wired:

```bash
mise exec -- cleanroom exec --backend darwin-vz -- docker version
```

With the gateway server running, guest `dockerd` automatically uses
`gateway.cleanroom.internal` as its Docker Hub mirror, backed by the embedded
OCI cache.

Run a container pull + execution smoke test:

```bash
mise exec -- cleanroom exec --backend darwin-vz -- docker run --rm --network none alpine:3.22 echo docker-example-ok
```

Expected output:

```text
docker-example-ok
```

When finished:

```bash
pkill -f "cleanroom serve"
```
