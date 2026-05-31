# Getting started

Use this guide when bringing a repository or Buildkite pipeline onto
Cleanroom. It starts with a local run, then shows where the shared control
plane, OIDC authentication, and GitHub App credentials fit.

## Mental model

Cleanroom has two configuration layers:

| Layer | Lives in | Owns |
|---|---|---|
| Repository policy | `cleanroom.yaml` or `.buildkite/cleanroom.yaml` | Sandbox image, resource floors, repository checkout defaults, deny-by-default network policy, Docker requirement, dependency and service setup |
| Runtime config | `~/.config/cleanroom/config.yaml` or the server's config path | Backend selection, control endpoint, TLS, auth, gateway credentials, cache peers, observability, lifecycle timers |

The `cleanroom` CLI is always a client. Local commands talk to the local
`cleanroom serve` daemon over a Unix socket by default. Shared servers expose
the same control API over HTTPS with authentication.

GitHub App support belongs to the host gateway runtime config. It lets the
server authenticate upstream Git requests for matching repositories without
placing credentials in the guest. It does not replace repository network
policy: a private GitHub checkout still needs policy entries such as
`github.com:443`, plus whatever package or API hosts the workload uses.

## 1. Install and check the host

```bash
curl -fsSL https://raw.githubusercontent.com/buildkite/cleanroom/main/scripts/install.sh | bash
cleanroom doctor
cleanroom daemon status
```

The installer puts `cleanroom` in `/usr/local/bin`, installs the VM helper
where needed, creates runtime config, and starts the daemon. `cleanroom doctor`
checks the selected backend, helper binaries, image tooling, and policy support
for the current host.

## 2. Prove a sandbox can run

Start with a repo-agnostic sandbox. This checks the daemon, backend, image
materialization, and guest execution before repository policy enters the
picture.

```bash
sandbox_id="$(cleanroom sandbox create --image ghcr.io/buildkite/cleanroom-base/alpine:latest)"
cleanroom exec --in "$sandbox_id" -- uname -a
cleanroom sandbox rm "$sandbox_id"
```

Use these diagnostics when a first run fails:

```bash
cleanroom status --last
cleanroom sandbox inspect --last
cleanroom execution inspect --last
cleanroom daemon logs
```

## 3. Run the Buildkite Agent example

The Buildkite Agent example keeps policy in this repository but checks out and
runs `buildkite/agent` inside the sandbox.

```bash
cd examples/buildkite-agent
cleanroom policy validate

cleanroom exec \
  --repo-url https://github.com/buildkite/agent.git \
  -- mise x -- go run . --version
```

The first run clones the repository, installs the declared `mise` toolchain,
downloads Go modules, and publishes reusable dependency-stage outputs. Later
runs against the same exact commit and policy can reuse those warmed outputs.

The example is public and does not require GitHub App credentials. Its network
allow-list is still representative: it allows GitHub for checkout and the exact
toolchain/module hosts needed during dependency bootstrap.

## 4. Add Cleanroom policy to your repository

In a real repository, commit `cleanroom.yaml` so CI and local developers use
the same policy. Start with the smallest set of hosts required for checkout and
the command you want to run.

```yaml
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/debian@sha256:28c3f638fabe1ed780f87b82cfb0c6dda2549c86b9e4edbe519e8250243411c5
  resources:
    memory: 8GiB
    disk: 16GiB
  network:
    default: deny
    allow:
      - github.com:443
      - proxy.golang.org:443
      - sum.golang.org:443
```

Validate before relying on it:

```bash
cleanroom policy validate
cleanroom exec -- go test ./...
```

Use a digest-pinned image in committed policy. To resolve or update an image
reference:

```bash
cleanroom image resolve ghcr.io/buildkite/cleanroom-base/debian:latest
cleanroom image bump-ref ghcr.io/buildkite/cleanroom-base/debian:latest
```

Top-level `cleanroom exec` and `cleanroom console` are repo-aware when run from
a Git checkout. They resolve the current remote and exact `HEAD`, then check out
that commit at `/workspace` in the guest. Dirty local files are not included
unless requested:

```bash
cleanroom exec --copy-in -- go test ./...
cleanroom exec --copy-out -- gofmt -w ./...
cleanroom exec --sync -- make generate
```

## 5. Configure a shared server

For a shared control plane, run the server on HTTPS and require bearer auth:

```bash
cleanroom serve --listen https://0.0.0.0:7777 \
  --tls-cert /etc/cleanroom/tls/server.pem \
  --tls-key /etc/cleanroom/tls/server.key
```

Server runtime config owns the auth policy and host-side upstream credentials:

```yaml
auth:
  required: true
  oidc:
    issuers:
      - name: buildkite
        issuer: https://agent.buildkite.com
        audiences:
          - https://cleanroom.example.com
        jwks_url: https://agent.buildkite.com/.well-known/jwks
  policy_file: /etc/cleanroom/auth-policy.yaml

gateway:
  credentials:
    github_app:
      app_id: "3817917"
      installation_id: "134770928"
      private_key_file: /etc/cleanroom/github-app.pem
      repo_prefixes:
        - buildkite/
```

Keep the GitHub App private key readable only by the daemon user. GitHub App
credentials authenticate the host gateway's upstream Git fetches for matching
repositories. The sandbox still sees normal Git URLs and still needs policy
allow entries for the upstream destinations.

See [Remote access](remote-access.md) for the full OIDC policy shape and
[Host gateway](gateway.md#credentials) for credential provider details.

## 6. Call the shared server from Buildkite

Request a short-lived Buildkite OIDC token for the Cleanroom server audience,
then pass it to the CLI:

```bash
buildkite-agent oidc request-token \
  --audience "https://cleanroom.example.com" \
  --subject-claim pipeline_id \
  --claim organization_id \
  > /tmp/cleanroom.jwt

cleanroom exec \
  --host https://cleanroom.example.com:7777 \
  --auth-token-file /tmp/cleanroom.jwt \
  --repo-url "$BUILDKITE_REPO" \
  -- go test ./...
```

Use `cleanroom auth check` before pointing a pipeline at a shared server. The
auth policy should bind immutable Buildkite IDs, then grant only the sandbox
and execution actions needed by that pipeline.

## 7. Decide where each setting belongs

| Need | Put it in |
|---|---|
| Allow `github.com:443` for checkout | Repository policy |
| Allow `proxy.golang.org:443` for dependency download | Repository policy |
| Require Docker inside the guest | Repository policy |
| Configure GitHub App credentials | Server runtime config |
| Choose `firecracker` or `darwin-vz` | Runtime config or an explicit `--backend` request flag |
| Require Buildkite OIDC before sandbox creation | Server runtime config |
| Set OTLP tracing or JSON logs | Server runtime config |
| Copy local uncommitted changes | Request flag such as `--copy-in` or `--sync` |

Repository policy should stay backend-neutral. Host-specific paths, TLS files,
credentials, cache peers, and daemon options belong in runtime config.
