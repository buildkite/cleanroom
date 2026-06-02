# Remote access

For an end-to-end view of how local and shared Cleanroom servers fit into the
control plane, start with [Control plane](control-plane.md).

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

Buildkite Pipelines can request tokens from the Buildkite agent OIDC issuer and
send them to Cleanroom. Configure the server to trust the Buildkite issuer and
to bind immutable Buildkite IDs to Cleanroom principals:

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
        required_claims:
          organization_id: "0184990a-477b-4fa8-9968-496074483k77"
  policy:
    bindings:
      - name: repo-bots
        when: >
          token.issuer == "buildkite" &&
          claims.pipeline_id == "0184990a-4782-42b5-afc1-16715b10b1l0"
        principal:
          id: "oidc:${token.issuer}:org:${claims.organization_id}:pipeline:${claims.pipeline_id}"
          scope: "org:${claims.organization_id}"
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
              - sandbox.terminate
              - sandbox.file.read
              - sandbox.file.write
              - sandbox.file.stat
              - execution.create
              - execution.get
              - execution.list
              - execution.attach
              - execution.inspect
              - execution.stream
              - execution.stdin.write
              - execution.stdin.close
              - execution.cancel
              - snapshot.create
              - snapshot.get
              - snapshot.list
              - snapshot.restore
              - snapshot.delete
            resources:
              - sandbox
              - execution
              - snapshot
```

The inline policy maps trusted token claims to Cleanroom principals and grants.
Use immutable Buildkite IDs, not reusable slugs, for principal IDs. Slugs and
branches are still useful for additional grant conditions, but should not define
ownership. For large or generated policies, `auth.policy_file` can point at a
separate YAML file with the same `bindings` shape; it is mutually exclusive with
`auth.policy`.

In a Buildkite command step, request a short-lived token for the Cleanroom
server audience and include the immutable claims used by the policy:

```bash
buildkite-agent oidc request-token \
  --audience "https://cleanroom.example.com" \
  --subject-claim pipeline_id \
  --claim organization_id \
  > /tmp/cleanroom.jwt
```

Clients send the token with either `CLEANROOM_AUTH_TOKEN`,
`--auth-token-env`, or `--auth-token-file`:

```bash
cleanroom sandbox create \
  --host https://cleanroom.example.com \
  --auth-token-file /tmp/cleanroom.jwt
```

Use `cleanroom auth check` before pointing a pipeline at a shared server. A
create check needs a request fixture; existing-resource checks need the expected
owner fields:

```bash
cleanroom auth check \
  --config /etc/cleanroom/config.yaml \
  --token-file /tmp/cleanroom.jwt \
  --action sandbox.create \
  --request create-request.json \
  --json

cleanroom auth check \
  --config /etc/cleanroom/config.yaml \
  --token-file /tmp/cleanroom.jwt \
  --action sandbox.get \
  --resource-id sbx_123 \
  --owner-principal-id oidc:buildkite:org:0184990a-477b-4fa8-9968-496074483k77:pipeline:0184990a-4782-42b5-afc1-16715b10b1l0 \
  --owner-scope org:0184990a-477b-4fa8-9968-496074483k77 \
  --json
```

With auth enabled, sandboxes, executions, snapshots, file operations, streams,
and port forwarding are exact-owner scoped. A principal can only access
resources created by the same derived principal ID. Existing snapshot records
without owner metadata are not readable through authenticated HTTP(S) calls.
