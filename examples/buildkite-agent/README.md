# Buildkite Agent Example

Runs the [buildkite/agent](https://github.com/buildkite/agent) test suite
inside a cleanroom sandbox with deny-by-default egress, explicit `mise`
dependency bootstrap, Go module resolution, and dependency-stage warmup.

## Prerequisites

Install cleanroom (`mise run install` from repository root). The example policy
declares enough guest resources for a large Go build:

```yaml
sandbox:
  resources:
    memory: 12GiB
    disk: 12GiB
```

## Usage

Run from this directory with a `cleanroom serve` instance running:

```bash
# Validate the policy
cleanroom policy validate

# Open an interactive shell in the checkout
cleanroom console \
  --backend darwin-vz \
  --repo-url https://github.com/buildkite/agent.git \
  --repo-commit 9eba5c5b83807b9aaaaffef6225be1f62c8d7d6c

# Run the test suite
cleanroom exec \
  --backend darwin-vz \
  --repo-url https://github.com/buildkite/agent.git \
  --repo-commit 9eba5c5b83807b9aaaaffef6225be1f62c8d7d6c \
  -- mise x -- go test -p 1 ./...

# Match the host-side gotestsum flow
cleanroom exec \
  --backend darwin-vz \
  --repo-url https://github.com/buildkite/agent.git \
  --repo-commit 9eba5c5b83807b9aaaaffef6225be1f62c8d7d6c \
  -- mise x -- go run gotest.tools/gotestsum@latest ./... -- -fastfail
```

## Network allow list

The `cleanroom.yaml` policy allows egress to the minimum set of hosts
needed for the explicit `mise exec -- go mod download` dependency bootstrap and
Go module resolution:

| Host | Why |
|---|---|
| `github.com`, `api.github.com` | Git clone and GitHub API |
| `release-assets.githubusercontent.com` | mise downloads release assets for managed tools |
| `dl.google.com` | upstream host validated by the gateway `/fetch/` route for Go SDK downloads |
| `proxy.golang.org`, `sum.golang.org` | upstream hosts validated by the gateway `/goproxy/` route and mirrored checksum database |
| `storage.googleapis.com` | Go proxy redirect target validated by the host-side goproxy client |
| `mise-versions.jdx.dev`, `mise.jdx.dev` | mise tool metadata resolution |
| `tuf-repo-cdn.sigstore.dev` | mise verifies GitHub artifact attestations |

## Notes

- First run is slow: git clone, mise tool install, and Go module download
- The example now targets the current `buildkite/agent` `HEAD` commit at the time of this update: `9eba5c5b83807b9aaaaffef6225be1f62c8d7d6c`
- The example policy uses the current multi-arch Debian base image digest `ghcr.io/buildkite/cleanroom-base/debian@sha256:28c3f638fabe1ed780f87b82cfb0c6dda2549c86b9e4edbe519e8250243411c5`
- Guest resource minimums are declared in `cleanroom.yaml`; larger host runtime defaults still win
- The policy sets:

  ```sh
  mise settings ruby.compile=false
  mise install
  mise exec -- go mod download
  ```

  and keys that stage on `.mise.toml`, `go.mod`, and `go.sum`, so a successful
  first run can publish a reusable dependency stage for later warm hits on the
  same exact commit and policy
- Cleanroom now injects `GOPROXY` and `MISE_GO_DOWNLOAD_MIRROR` automatically
  when the relevant upstream hosts are allowlisted, so `go mod download` and
  `mise use go@...` warm the shared gateway cache instead of fetching directly
- `ruby.compile=false` avoids source-building Ruby during the dependency stage while still allowing `mise install` to satisfy the repo's current tool declarations
- Use `mise x -- go ...` for execution commands so the installed Go toolchain is placed on `PATH`
- `go test -p 1` avoids OOM kills on constrained guest memory
