# Storage

Cleanroom stores several different kinds of host-side state. They have different
owners, lifetimes, and sizing rules, so a storage knob for one layer should not
be treated as a prewarm or cache-size knob for another.

Related docs:

- [Caching](caching.md) covers dependency and service cache semantics.
- [Operations](operations.md) covers `cleanroom system df` and prune commands.
- [Darwin VZ Backend](backend/darwin-vz.md) covers macOS-specific rootfs and
  snapshot driver behavior.
- [Spec](spec.md) defines the repository-facing `sandbox.resources.disk`
  contract.

## Model

For an image-backed sandbox, the root filesystem path is layered like this:

```text
OCI image
  -> image rootfs artifact
  -> prepared runtime rootfs
  -> writable sandbox rootfs
```

Declared dependency and service outputs use a separate path:

```text
declared output path
  -> cache-output volume
  -> cache-output snapshot
```

Stage caches and user snapshots capture reusable rootfs or output-volume state
after a sandbox has already run. They are metadata-backed storage records, not
image cache entries.

## RootFS Artifacts

### Image RootFS Artifact

The image manager materializes an OCI image into an ext4 rootfs artifact under
the image cache. It is keyed by image digest and represents image contents.

This artifact is not sized from `minimum_rootfs_bytes` or
`sandbox.resources.disk`. It only needs enough space for image contents and
materialization headroom.

### Prepared Runtime RootFS

The prepared runtime rootfs is a shared immutable image-derived artifact. On
`darwin-vz`, Cleanroom copies the image rootfs artifact and injects the guest
runtime files needed to boot and execute commands.

The prepared runtime rootfs is keyed by:

- image digest
- guest agent hash
- host architecture
- guest runtime versions

It is not keyed by writable disk floors such as `minimum_rootfs_bytes` or
`sandbox.resources.disk`. Changing a writable disk floor should not force a new
prepared runtime rootfs cache entry.

### Writable Sandbox RootFS

The writable sandbox rootfs is the per-sandbox or per-execution copy attached to
the VM read-write. This is where writable disk capacity applies.

The effective writable rootfs minimum is resolved from the largest applicable
floor:

| Source | When It Applies |
|--------|-----------------|
| `config` | backend runtime config sets `minimum_rootfs_bytes` |
| `policy` | `sandbox.resources.disk` is larger than the current floor |
| `repository_bootstrap` | repository bootstrap needs at least 8 GiB |
| `docker_repository_bootstrap` | repository bootstrap with Docker required needs at least 16 GiB |

If no floor applies, the backend uses the image or snapshot size as-is.

On `darwin-vz`, rootfs resizing happens after the writable clone is created and
before VM boot. The launch phase is reported as
`rootfs_minimum_size_resize`. The run observation also records
`rootfs_minimum_bytes` and `rootfs_minimum_source` when available, and traces use
`cleanroom.rootfs.minimum_bytes` and `cleanroom.rootfs.minimum_source`.

## Cache Output Volumes

Cache output volumes back declared dependency and service output paths. They are
separate from the rootfs floor and are controlled by cache-output sizing.

Use these for dependency or service state such as:

- `${HOME}/go/pkg/mod`
- package manager stores
- `/var/lib/docker`
- database files built by a service block

The default cache-output volume floor is 16 GiB. Runtime config can override it
per backend with `minimum_cache_output_volume_bytes`.

Cleanroom may also use the previous successful output size for the same block
shape as a sizing hint when creating a new cold volume. Existing on-disk volumes
keep their current size; the floor applies when a volume is created or restored
from an older snapshot.

## Configuration

Most image-only sandboxes should not set a global rootfs floor. Let the image
size define the writable rootfs size unless the workload actually needs more
space.

Use `sandbox.resources.disk` for repository-declared workload requirements:

```yaml
sandbox:
  resources:
    disk: 16GiB
```

Use backend `minimum_rootfs_bytes` only as an operator-wide writable rootfs
floor for that host:

```yaml
backends:
  darwin-vz:
    minimum_rootfs_bytes: 16GiB
```

Use `minimum_cache_output_volume_bytes` for declared output volume defaults:

```yaml
backends:
  darwin-vz:
    minimum_cache_output_volume_bytes: 16GiB
```

These settings are independent. Increasing `minimum_rootfs_bytes` should not
increase prepared runtime rootfs cache entries, and increasing
`minimum_cache_output_volume_bytes` should not resize the rootfs.

## Backend Notes

`darwin-vz` uses sparse ext4 files and APFS clonefile-backed copies when the
`apfs` snapshot driver is selected. Logical capacity can be larger than current
host disk use, but filesystem metadata and actual guest writes still consume
host storage.

`firecracker` uses the same backend-neutral resource contract, but the physical
storage behavior depends on the configured snapshot driver, such as `file` or
`zfs`.

## Inspect And Prune

Use the system commands to see where host storage is going:

```bash
cleanroom system df
cleanroom system df --json
cleanroom system prune --dry-run
```

Use `cleanroom system prune --all --older-than 7d` only when the dry run shows
the expected reclaimable entries. Prune protects active sandboxes and does not
delete explicit snapshots by default.

When sandbox creation is slow, check:

- `cleanroom system df` for unusually large runtime-rootfs or snapshot caches
- launch phase metrics for `rootfs_resolve`, `rootfs_minimum_size_resize`, and
  cache-output phases
- `rootfs_minimum_bytes` and `rootfs_minimum_source` in the darwin-vz run
  observation file or trace attributes
- whether the request is image-only, repository-backed, Docker-backed, or
  policy-declared with `sandbox.resources.disk`
