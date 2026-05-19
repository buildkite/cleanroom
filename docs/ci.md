# CI Setup (Buildkite)

Cleanroom uses Buildkite for tests, E2E runs, and release publishing.

The public repo owns the pipeline definition and the product-facing CI scripts:

- `.buildkite/pipeline.yml`
- `scripts/ci-cleanroom-e2e.sh`
- `scripts/ci-darwin-vz-e2e.sh`
- `scripts/ci-darwin-vz-filehandle-e2e.sh`
- `scripts/ci-macos-release-pkg.sh`
- `scripts/ci-darwin-vz-kernel-release.sh`
- `scripts/ci-buildkite-release.sh`
- `images/`

On tagged builds, `scripts/ci-darwin-vz-kernel-release.sh` builds the
experimental Apple Silicon `darwin-vz` minimal rootfs-profile kernel and uploads
it as Buildkite artifacts under `release-extra/kernels/`. The publish job then
uploads those files as direct assets on the same GitHub Release as the Cleanroom
archives and macOS packages. Runtime managed-kernel resolution reads the release
manifest from the matching tag for released builds and from the latest published
release for dev builds.

Private CI infrastructure, bootstrap, and host recovery documentation now lives
in the sibling repo `../cleanroom-ops`, especially `../cleanroom-ops/docs/ci.md`.

Use that repo for:

- Terraform-managed CI and prod hosts
- bootstrap scripts and rerun tooling
- signer queue and release secret setup
- SSM-based recovery and host lifecycle operations
