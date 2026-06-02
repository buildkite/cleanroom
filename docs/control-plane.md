# Control plane

Cleanroom uses one control plane for local and remote operation. Local commands
are not a separate execution path; they are CLI calls to a local server endpoint.

## Components

| Component | Role |
|---|---|
| CLI and API clients | Resolve the control endpoint, load repository policy, package requested local changes when asked, and call the Cleanroom API |
| `cleanroom serve` | Authoritative server for sandbox lifecycle, execution state, policy validation, auth, observability, and backend dispatch |
| Backend adapter | Implements VM creation, file operations, execution, suspend/resume, and backend capability checks for `firecracker` or `darwin-vz` |
| Guest agent | Runs inside each microVM and performs command execution, file transfer, and interactive session work requested by the server |
| Host gateway | Mediates allowed guest access to Git, OCI registries, Go modules, RubyGems, immutable fetches, and host-side upstream credentials |
| Repository policy | Backend-neutral workload contract committed with the repository |
| Runtime config | Host or server-specific configuration for endpoints, auth, credentials, caches, lifecycle, and observability |

The default local endpoint is a Unix socket. HTTP loopback is useful for local
development. Shared servers should use HTTPS with `auth.required: true`.

## Request lifecycle

For a typical `cleanroom exec`:

1. The CLI resolves the server endpoint from `--host`, `CLEANROOM_HOST`,
   runtime config, or the default socket.
2. The CLI loads and compiles repository policy from `cleanroom.yaml` or
   `.buildkite/cleanroom.yaml`.
3. The CLI resolves repository metadata, including the remote URL and exact
   commit, and packages local changes only when flags such as `--copy-in` or
   `--sync` request that.
4. The server authenticates and authorizes the caller when auth is enabled.
5. The server validates and persists the compiled policy for the sandbox
   lifetime.
6. The selected backend materializes the image, creates the microVM, starts the
   guest agent, and checks that required policy capabilities can be enforced.
7. Repository checkout and requested changeset application happen in the guest
   workspace.
8. Dependency and service blocks run at sandbox creation time when configured.
9. The server creates the requested execution and streams output back through
   the control API.
10. The sandbox remains available for later commands until it is removed or the
    caller chooses a one-shot path that cleans it up.

The compiled policy is immutable for the sandbox lifetime. A later policy file
change affects newly created sandboxes, not already running ones.

## Auth and ownership

Local Unix-socket use is intended for trusted local callers. Remote control
plane access uses HTTPS bearer authentication.

With OIDC enabled, the server:

1. Verifies the token issuer, audience, signature, lifetime, and required
   claims.
2. Maps trusted claims to a Cleanroom principal.
3. Evaluates grants for the requested action and request attributes, such as
   repository remote URL, backend, resource floors, and network default.
4. Records the principal as the owner of created sandboxes, executions, and
   snapshots.

Authenticated resource access is exact-owner scoped. A caller can only manage
resources owned by the same derived principal. Grants can allow or deny actions
on resources owned by that principal, but they cannot grant access to resources
owned by a different derived principal.

## Host gateway and credentials

The host gateway is part of the server runtime, not the guest. It receives
traffic from sandboxes, identifies the sandbox, checks the active compiled
policy, then decides whether to fetch upstream or deny the request.

Supported gateway routes include:

| Route | Purpose |
|---|---|
| `/git/` | Cache-backed Git smart-HTTP and mirror-backed Git proxying |
| `/registry/` | OCI registry pull-through route |
| `/v2/` | Docker Hub-compatible mirror for guest `dockerd` |
| `/goproxy/` | Go module proxy and checksum database mirror |
| `/rubygems/` | Bundler/RubyGems mirror |
| `/fetch/` | Immutable artifact downloads such as Go SDK tarballs |

Gateway credentials live in runtime config or trusted server environment, never
in `cleanroom.yaml` and never in the guest environment.

GitHub App credentials are one credential provider for upstream Git requests:

```yaml
gateway:
  credentials:
    github_app:
      app_id: "3817917"
      installation_id: "134770928"
      private_key_file: /etc/cleanroom/github-app.pem
      repo_prefixes:
        - buildkite/
```

When a sandbox runs `git clone https://github.com/buildkite/private-repo.git`,
the guest command remains unchanged. Cleanroom rewrites matching Git traffic to
`gateway.cleanroom.internal`, the gateway validates that `github.com:443` is
allowed by the active policy, and the host-side gateway obtains the upstream
credential if the repository matches `repo_prefixes`.

GitHub App credentials answer "how can the host authenticate upstream?" They do
not answer "is this sandbox allowed to reach GitHub?" The allow decision still
comes from repository policy.

## Configuration boundaries

Keep repository policy portable:

```yaml
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/debian@sha256:28c3f638fabe1ed780f87b82cfb0c6dda2549c86b9e4edbe519e8250243411c5
  resources:
    memory: 8GiB
  network:
    default: deny
    allow:
      - github.com:443
      - proxy.golang.org:443
      - sum.golang.org:443
```

Keep host concerns in runtime config:

```yaml
control_host: unix:///run/user/501/cleanroom/cleanroom.sock
default_backend: darwin-vz
auth:
  required: true
gateway:
  credentials:
    github_app:
      app_id: "3817917"
      installation_id: "134770928"
      private_key_file: /etc/cleanroom/github-app.pem
      repo_prefixes:
        - buildkite/
```

This split lets the same repository policy run on local macOS, local Linux, and
a shared CI server while each host keeps its own backend, credentials, and
operational settings.

## Related docs

- [Getting started](getting-started.md) for the first local and CI flows
- [Policy](policy.md) for `cleanroom.yaml`
- [Remote access](remote-access.md) for HTTPS and OIDC auth
- [Host gateway](gateway.md) for route and credential details
- [API](api.md) for the ConnectRPC service surface
