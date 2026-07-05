# CI Setup (Buildkite)

Cleanroom uses Buildkite for tests and release publishing.

The public repo owns the pipeline definition and the product-facing CI scripts:

- `.buildkite/pipeline.yml`
- `scripts/ci-bake-smoke.sh`
- `scripts/ci-buildkite-release.sh`
- `images/`

The bake smoke runs the full bake-and-restore loop against a pinned spore
release (see `mise.toml`): it compiles a policy, bakes a spore for a scratch
repository, restores it with `spore run --from`, rebakes to confirm the
idempotent no-op path, forks the artifact, and verifies provenance.

The protobuf schema is linted with Buf on every build and published to
[buf.build/buildkite/cleanroom](https://buf.build/buildkite/cleanroom) after
tests pass on `main` and tag builds. The publish step uses
`buf push --git-metadata`, so Buildkite must expose a BSR token as `BUF_TOKEN`
on those builds.

Cleanroom does not build kernels or VM images during its release pipeline; the
VM runtime is [spore](https://github.com/sporevm/sporevm), and the release
ships the single `cleanroom` binary.

Private CI infrastructure, bootstrap, and host recovery documentation now lives
in the sibling repo `../cleanroom-ops`, especially `../cleanroom-ops/docs/ci.md`.

Use that repo for:

- Terraform-managed CI and prod hosts
- bootstrap scripts and rerun tooling
- signer queue and release secret setup
- SSM-based recovery and host lifecycle operations
