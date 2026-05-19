# Caching

Cleanroom has two useful cache layers for normal users:

- transport caching through the host gateway, backed by
  [`content-cache`](https://github.com/buildkite/content-cache)
- environment caching from declared dependency and service outputs

Both are designed for CI-style reuse without giving the guest broad host access
or upstream credentials.

## Transport Cache

Allowed dependency traffic goes through the host gateway when Cleanroom has a
route for that protocol:

| Traffic | Gateway Route |
|---------|---------------|
| Git smart HTTP | `/git/` |
| OCI registries | `/registry/` |
| Docker Hub pull-through mirror | `/v2/` |
| Go modules and checksum database | `/goproxy/` |
| RubyGems and Bundler mirror traffic | `/rubygems/` |
| Immutable downloads | `/fetch/` |

The guest still asks for normal upstream names such as `github.com`,
`ghcr.io`, `proxy.golang.org`, or `rubygems.org`. Cleanroom rewrites supported
traffic to `gateway.cleanroom.internal`, checks policy, and lets the gateway
fetch or serve cached content.

See [Host Gateway](gateway.md) for route-level details.

## Dependency Outputs

Dependency blocks run at sandbox creation and publish declared outputs for later
warm runs:

```yaml
sandbox:
  dependencies:
    - name: go-modules
      command: go mod download
      inputs:
        files: [go.mod, go.sum]
      outputs:
        dirs:
          - ${HOME}/go/pkg/mod
```

The cache key includes policy, image, repository input, declared input files,
block command, block environment, and declared outputs. If those inputs still
match, Cleanroom can restore the output instead of running the setup command
again.

Use dependency blocks for package installs, tool downloads, and repo-local setup
that should be deterministic from committed files.

## Service Outputs

Service blocks prepare on-disk state for services. Live services should still
start in `sandbox.run.before` before each execution:

```yaml
sandbox:
  docker:
    required: true
  services:
    - name: database
      command: |
        docker compose up -d postgres valkey
        bin/rails db:prepare
        docker compose stop postgres valkey
      inputs:
        files: [docker-compose.yml, db/schema.rb]
      outputs:
        dirs: [/var/lib/docker]
  run:
    before: docker compose up -d postgres valkey
```

Use service blocks for database files, Docker state, fixture stores, or other
guest-local state that is expensive to rebuild but safe to reuse from declared
inputs.

## Cache Output Volume Size

See [Storage](storage.md#cache-output-volumes) for how cache-output volumes fit
alongside image artifacts, prepared runtime rootfs entries, writable rootfs
copies, and snapshots.

Cache output volumes are sparse ext4 images on the host. When Cleanroom creates
a new volume — or restores one from an older snapshot — it enforces a minimum
size. The default is 16 GiB.
<!-- tracks cacheoutput.DefaultVolumeMinimumBytes -->

Because the images are sparse, the default is logical capacity rather than
immediate host disk use. Host disk is consumed by actual writes and filesystem
metadata. For example, a 16 GiB Docker cache-output volume can occupy much less
than 16 GiB on the host until Docker writes that data.

Cleanroom also uses prior successful snapshots for the same block shape as a
sizing hint. If a block's declared inputs change and it needs to rebuild, the
next cold cache-output volume can be created with enough logical capacity for
the previous successful output's allocated bytes plus headroom.

Most users should not need to configure cache-output sizing. If a host needs a
larger or smaller floor for every cache-output volume, set
`minimum_cache_output_volume_bytes` in runtime config:

```yaml
backends:
  darwin-vz:
    minimum_cache_output_volume_bytes: 16GiB
```

```yaml
backends:
  firecracker:
    minimum_cache_output_volume_bytes: 16GiB
```
Existing on-disk writable volumes keep their current size; the new minimum only
applies when a volume is first created or restored from a snapshot that predates
the setting.

## Host Cache Peers

Runtime config can point Cleanroom at trusted cache peers:

```yaml
cache:
  peers:
    - url: https://cleanroom-cache-1.example.com
      token_env: CLEANROOM_CACHE_TOKEN
```

Peer configuration belongs in runtime config, not `cleanroom.yaml`, because it
describes host topology rather than repository intent.

## Inspect And Prune

Use storage commands to see and clean host-side state:

```bash
cleanroom system df
cleanroom system prune --dry-run
cleanroom system prune --all --older-than 7d
```

See [Operations](operations.md) for storage command behavior.
