# Host gateway

The Cleanroom server runs a host gateway that provides mediated access to
external services for sandboxes. The gateway handles git clones, package
registry requests, secret injection, and metadata. Credentials are held
host-side and injected on the upstream leg, so tokens never enter the sandbox.

Currently supported on the `firecracker` backend.

## Endpoints

| Path | Purpose |
|------|---------|
| `/git/` | Git smart-HTTP proxy with policy-scoped URL rewrites |
| `/registry/` | Package registry proxy |
| `/secrets/` | Secret injection endpoint |
| `/meta/` | Sandbox metadata |

## Git proxy

Cleanroom rewrites clone URLs to the host gateway when the target host is in
`sandbox.network.allow`. Clone commands run unchanged inside the sandbox:

```bash
cleanroom exec -- git clone https://github.com/org/repo.git
```

The gateway resolves the target host from the request path, validates it against
the sandbox's compiled policy, and proxies the git smart-HTTP protocol upstream.

Allowed host example (from this repo's policy):

```bash
cleanroom exec -- git ls-remote https://github.com/buildkite/cleanroom.git HEAD
```

Denied host example (not in `sandbox.network.allow`):

```bash
cleanroom exec -- git ls-remote https://gitlab.com/gitlab-org/gitlab.git HEAD
```

## Credentials

Host-side credentials are provided via environment variables:

| Variable | Purpose |
|----------|---------|
| `CLEANROOM_GITHUB_TOKEN` | GitHub authentication |
| `CLEANROOM_GITLAB_TOKEN` | GitLab authentication |

Credentials are injected into upstream requests by the gateway. They are never
exposed to the guest environment.

## Configuration

The gateway listens on `:8170` by default. Use `--gateway-listen` to change:

```bash
cleanroom serve --gateway-listen :0    # ephemeral port
```
