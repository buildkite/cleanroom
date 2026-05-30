# Remote access

The Cleanroom server supports HTTP and HTTPS listener modes for remote access.

## HTTP

HTTP listeners are accepted only on loopback addresses. That remains true when
bearer authentication is enabled, because bearer tokens are not accepted on
non-loopback plain HTTP. Use HTTP for local development only.

```bash
cleanroom serve --listen http://127.0.0.1:7777
```

Use HTTPS plus `auth.required` for shared servers.

## HTTPS

See [TLS](tls.md) for certificate setup.

```bash
cleanroom serve --listen https://0.0.0.0:7777 \
  --tls-cert /path/to/server.pem \
  --tls-key /path/to/server.key
```

Non-loopback HTTPS listeners require `auth.required: true`.

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
        required_claims:
          repository_owner_id: "123456"
  policy:
    bindings:
      - name: repo-bots
        when: claims.repository_id == "987654"
        principal:
          id: "oidc:${token.issuer}:owner:${claims.repository_owner_id}:repo:${claims.repository_id}"
          scope: "owner:${claims.repository_owner_id}"
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

The inline policy maps trusted token claims to Cleanroom principals and grants.
Use immutable provider claim IDs, not reusable slugs, for principal IDs. For
large or generated policies, `auth.policy_file` can point at a separate YAML
file with the same `bindings` shape; it is mutually exclusive with
`auth.policy`.

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
