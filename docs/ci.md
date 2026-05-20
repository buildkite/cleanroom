# CI Setup (Buildkite)

Cleanroom uses Buildkite for tests, E2E runs, and release publishing.

The public repo owns the pipeline definition and the product-facing CI scripts:

- `.buildkite/pipeline.yml`
- `scripts/ci-with-host-lock.sh`
- `scripts/ci-cleanroom-e2e.sh`
- `scripts/ci-darwin-vz-e2e.sh`
- `scripts/ci-darwin-vz-filehandle-e2e.sh`
- `scripts/ci-examples-firecracker.sh`
- `scripts/ci-examples-darwin-vz.sh`
- `scripts/ci-macos-release-pkg.sh`
- `scripts/ci-darwin-vz-kernel-release.sh`
- `scripts/ci-buildkite-release.sh`
- `images/`

Backend E2E and examples smoke jobs use `scripts/ci-with-host-lock.sh` to
serialize VM work per physical host with Buildkite agent locks instead of
pipeline-wide concurrency groups. Agents in the `cleanroom` and `cleanroom-mac`
queues must run with the Buildkite `agent-api` experiment enabled so
`buildkite-agent lock acquire` and `buildkite-agent lock release` are available.

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
