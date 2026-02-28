# Docker-In-Guest Example

This example boots a sandbox from the Docker image, auto-starts `dockerd` in guest init, and runs `docker` commands inside the sandbox.

## Prerequisites

From repository root:

```bash
mise run install
```

## Files

- `cleanroom.yaml`: digest-pinned Docker image ref and a deny-by-default network allowlist for Docker Hub pull endpoints.

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
