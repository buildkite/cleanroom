# Buildkite Agent Example

Runs the [buildkite/agent](https://github.com/buildkite/agent) test suite
inside a cleanroom sandbox with deny-by-default egress, explicit `mise`
dependency bootstrap, Go module resolution, and dependency-stage warmup.

## Prerequisites

Install cleanroom (`mise run install` from repository root) and ensure
your runtime config has enough resources for a large Go build:

```yaml
# ~/.config/cleanroom/config.yaml
backends:
  darwin-vz:
    memory_mib: 4096
    minimum_rootfs_bytes: 12GiB
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
  --repo-commit 704a7e24737231681395b58604ca5174d2335712

# Run the test suite
cleanroom exec \
  --backend darwin-vz \
  --repo-url https://github.com/buildkite/agent.git \
  --repo-commit 704a7e24737231681395b58604ca5174d2335712 \
  -- go test -p 1 ./...
```

## Network allow list

The `cleanroom.yaml` policy allows egress to the minimum set of hosts
needed for the explicit `mise exec -- go mod download` dependency bootstrap and
Go module resolution:

| Host | Why |
|---|---|
| `github.com`, `api.github.com` | Git clone and GitHub API |
| `release-assets.githubusercontent.com` | mise downloads golangci-lint from GitHub releases |
| `dl.google.com` | upstream host validated by the gateway `/fetch/` route for Go SDK downloads |
| `proxy.golang.org`, `sum.golang.org` | upstream hosts validated by the gateway `/goproxy/` route and mirrored checksum database |
| `storage.googleapis.com` | Go proxy redirect target validated by the host-side goproxy client |
| `mise-versions.jdx.dev`, `mise.jdx.dev` | mise tool metadata resolution |

## Notes

- First run is slow: git clone, mise tool install, and Go module download
- The example policy sets `sandbox.dependencies.command: [mise, exec, --, go, mod, download]`
  and keys that stage on `.mise.toml`, `go.mod`, and `go.sum`, so a successful
  first run can publish a reusable dependency stage for later warm hits on the
  same exact commit and policy
- Cleanroom now injects `GOPROXY` and `MISE_GO_DOWNLOAD_MIRROR` automatically
  when the relevant upstream hosts are allowlisted, so `go mod download` and
  `mise use go@...` warm the shared gateway cache instead of fetching directly
- `go test -p 1` avoids OOM kills on constrained guest memory
