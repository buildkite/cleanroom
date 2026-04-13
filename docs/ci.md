# CI Setup (Buildkite)

Cleanroom uses Buildkite for tests, E2E runs, and release publishing.

The public repo owns the pipeline definition and the product-facing CI scripts:

- `.buildkite/pipeline.yml`
- `scripts/ci-cleanroom-e2e.sh`
- `scripts/ci-darwin-vz-e2e.sh`
- `scripts/ci-darwin-vz-filehandle-e2e.sh`
- `scripts/ci-macos-release-pkg.sh`
- `scripts/ci-buildkite-release.sh`
- `images/`

Private CI infrastructure, bootstrap, and host recovery documentation now lives
in the sibling repo `../cleanroom-ops`, especially `../cleanroom-ops/docs/ci.md`.

Use that repo for:

- Terraform-managed CI and prod hosts
- bootstrap scripts and rerun tooling
- signer queue and release secret setup
- SSM-based recovery and host lifecycle operations
