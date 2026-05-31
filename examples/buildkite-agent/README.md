# Buildkite Agent Example

Checks out [buildkite/agent](https://github.com/buildkite/agent), warms its
`mise` toolchain and Go modules, then runs the agent CLI in a Cleanroom sandbox
with deny-by-default egress.

## Prerequisites

Install Cleanroom with the main installer. The installer starts the daemon.

This policy declares enough guest resources for a large Go build:

```yaml
sandbox:
  resources:
    memory: 12GiB
    disk: 12GiB
```

## Usage

Run from this directory:

```bash
cleanroom policy validate

cleanroom exec \
  --repo-url https://github.com/buildkite/agent.git \
  -- mise x -- go run . --version
```

That builds and runs the agent CLI without needing a Buildkite token. To inspect
the checkout before running commands, open a console:

```bash
cleanroom console \
  --repo-url https://github.com/buildkite/agent.git
```

To run tests, use the repo's Go test path:

```bash
cleanroom exec \
  --repo-url https://github.com/buildkite/agent.git \
  -- mise x -- go test -p 1 ./...
```

Starting a connected worker is the next step, and requires the usual Buildkite
agent token and `buildkite-agent start` flags.

## Network Allow List

The `cleanroom.yaml` policy allows egress to the minimum set of hosts needed
for repository checkout, the explicit `mise exec -- go mod download`
dependency bootstrap, and Go module resolution.

This section is still current with GitHub App support. GitHub App credentials
are host-side gateway credentials; they authenticate upstream Git requests for
matching repositories. They do not replace the sandbox network allow-list. A
private GitHub checkout still needs `github.com:443` in policy, while the
server runtime config decides whether the gateway authenticates with a GitHub
App, a token, or no credential.

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
- For the broader flow from local install to shared Buildkite usage, see
  [Getting started](../../docs/getting-started.md).
