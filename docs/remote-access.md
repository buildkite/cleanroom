# Remote access

The Cleanroom server supports HTTP and HTTPS listener modes for remote access.

## HTTP

HTTP remote access is intended for trusted networks or local development.

```bash
cleanroom serve --listen http://0.0.0.0:7777
```

When `auth.required` is enabled, bearer-token authentication is only accepted
on HTTPS or loopback HTTP listeners. Use HTTPS for shared servers.

## HTTPS

See [TLS](tls.md) for certificate setup.

```bash
cleanroom serve --listen https://0.0.0.0:7777 \
  --tls-cert /path/to/server.pem \
  --tls-key /path/to/server.key
```

## OIDC bearer authentication

Shared Cleanroom servers can require OIDC JWT bearer tokens for HTTP(S)
control-plane calls:

```yaml
auth:
  required: true
  oidc:
    issuers:
      - name: github-actions
        issuer: https://token.actions.githubusercontent.com
        audiences:
          - cleanroom
        jwks_url: https://token.actions.githubusercontent.com/.well-known/jwks
  policy_file: auth-policy.yaml
```

The policy file maps trusted token claims to Cleanroom principals and grants.
Principals are not listed in the main server config:

```yaml
bindings:
  - name: repo-bots
    when: claims.repository == "buildkite/cleanroom"
    principal:
      id: "oidc:${token.issuer}:${token.subject}"
      scope: "repo:${claims.repository}"
    grants:
      - name: create-cleanroom-sandboxes
        actions:
          - sandbox.create
        resources:
          - sandbox
        condition: >
          request.backend == "darwin-vz" &&
          request.repository.remote_url == "https://github.com/buildkite/cleanroom.git"
      - name: manage-owned-resources
        actions:
          - sandbox.get
          - sandbox.list
          - execution.create
          - execution.get
          - execution.list
          - snapshot.create
          - snapshot.get
          - snapshot.list
          - snapshot.restore
        resources:
          - sandbox
          - execution
          - snapshot
```

Clients send the token with either `CLEANROOM_AUTH_TOKEN`,
`--auth-token-env`, or `--auth-token-file`:

```bash
CLEANROOM_AUTH_TOKEN="$TOKEN" \
  cleanroom sandbox create --host https://server.example.com:7777
```

With auth enabled, sandboxes, executions, snapshots, file operations, streams,
and port forwarding are exact-owner scoped. A principal can only access
resources created by the same derived principal ID. Existing snapshot records
without owner metadata are not readable through authenticated HTTP(S) calls.
