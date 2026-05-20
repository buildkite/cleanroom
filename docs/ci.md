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
serialize VM work per physical host instead of pipeline-wide concurrency
groups. The wrapper prefers Buildkite agent locks when the agent's `agent-api`
experiment is enabled, and falls back to OS-level host file locks otherwise.
Native Buildkite lock acquisition waits up to 45 minutes by default; set
`CLEANROOM_BUILDKITE_LOCK_WAIT_TIMEOUT` or `BUILDKITE_LOCK_WAIT_TIMEOUT` to
override that duration.
Fallback locks live under `/tmp/cleanroom-ci-host-locks` by default so separate
Buildkite jobs still coordinate even when each job has its own `TMPDIR`; set
`CLEANROOM_CI_HOST_LOCK_DIR` to override that path on a host.
Because Buildkite checks out and cleans the shared agent worktree before the
command starts, the wrapper clones the checked-out commit into a temporary
per-job workspace before running the backend smoke script. Set
`CLEANROOM_CI_ISOLATE_WORKSPACE=0` to disable this, or
`CLEANROOM_CI_WORKSPACE_PARENT` to choose the temporary workspace parent.

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
