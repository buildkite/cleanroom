# CI Setup (Buildkite)

Cleanroom uses Buildkite for tests, E2E runs, and release publishing.

The public repo owns the pipeline definition and the product-facing CI scripts:

- `.buildkite/pipeline.yml`
- `scripts/ci-with-host-lock.sh`
- `scripts/ci-auth-oidc-smoke.sh`
- `scripts/ci-cleanroom-e2e.sh`
- `scripts/ci-darwin-vz-e2e.sh`
- `scripts/ci-darwin-vz-filehandle-e2e.sh`
- `scripts/ci-examples-firecracker.sh`
- `scripts/ci-examples-darwin-vz.sh`
- `scripts/ci-macos-release-pkg.sh`
- `scripts/ci-buildkite-release.sh`
- `images/`

Backend E2E and examples smoke jobs use `scripts/ci-with-host-lock.sh` to
serialize VM work per physical host instead of pipeline-wide concurrency
groups. The wrapper uses Buildkite agent locks, so the CI agents must run with
the `agent-api` experiment enabled. Lock acquisition waits up to 45 minutes by
default; set `CLEANROOM_BUILDKITE_LOCK_WAIT_TIMEOUT` or
`BUILDKITE_LOCK_WAIT_TIMEOUT` to override that duration.
CI agent configuration is responsible for giving concurrent jobs distinct
checkout directories before the command wrapper starts.

The auth OIDC smoke runs on hosted agents and requests a short-lived token from
the Buildkite agent issuer. By default it derives the expected organization and
pipeline IDs from the minted token, then validates the same token through
`cleanroom auth check` using the live Buildkite JWKS. It also checks
owner-scoped `sandbox.get` allow/deny decisions with supplied owner metadata.
Set
`CLEANROOM_AUTH_OIDC_ORGANIZATION_ID` or `CLEANROOM_AUTH_OIDC_PIPELINE_ID` if
the pipeline should fail when those immutable IDs drift.

The protobuf schema is linted with Buf on every build and published to
`buf.build/buildkite/cleanroom` after tests pass on `main` and tag builds. The
publish step uses `buf push --git-metadata`, so Buildkite must expose a BSR
token as `BUF_TOKEN` on those builds.

Cleanroom no longer builds managed `darwin-vz` kernels during its release
pipeline. The experimental Apple Silicon `darwin-vz` minimal rootfs-profile
kernel is published from the `buildkite/cleanroom-kernels` project, and runtime
managed-kernel resolution reads the pinned kernel release manifest.

Private CI infrastructure, bootstrap, and host recovery documentation now lives
in the sibling repo `../cleanroom-ops`, especially `../cleanroom-ops/docs/ci.md`.

Use that repo for:

- Terraform-managed CI and prod hosts
- bootstrap scripts and rerun tooling
- signer queue and release secret setup
- SSM-based recovery and host lifecycle operations
