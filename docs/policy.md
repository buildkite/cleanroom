# Policy

Cleanroom reads repository policy from `cleanroom.yaml`, then
`.buildkite/cleanroom.yaml`.

Policy is repo intent: the image, resources, network access, warmup, and
mediated credentials a baked spore needs. `cleanroom compile` translates the
enforceable subset into `spore create` arguments and **fails closed** on
anything SporeVM cannot enforce — a policy that validates but cannot be
enforced never bakes.

## Minimal Policy

```yaml
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny
    allow:
      - github.com:443
      - ghcr.io:443
```

Validate policy before relying on it:

```bash
cleanroom policy validate
cleanroom policy validate --json
```

Use a digest-pinned image ref in committed policy. The digest is recorded in
the baked spore's provenance and checked by `cleanroom verify`.

## Network

Cleanroom requires deny-by-default networking:

```yaml
sandbox:
  network:
    default: deny
    allow:
      - api.github.com:443
      - host: registry.npmjs.org
        ports: [443]
```

Rules are host and port rules, not URLs, and every rule must name a host and
at least one port. Compile renders each rule as a `spore create
--allow-host-port` argument; SporeVM learns destination IPs from DNS answers
and enforces them as IP:port pairs on every resume and fork of the baked
spore.

## Resources

```yaml
sandbox:
  resources:
    vcpus: 4
    memory: 8gb
```

Sizes use decimal units (`1gb` = 10^9 bytes); compile rounds memory up to
SporeVM's 16KiB page alignment. `resources.disk` is not yet translated and
fails compile.

## Warmup

Warmup commands run once inside the builder VM during `cleanroom bake`, after
the workspace is copied in. Use them to install dependencies and warm caches
so the captured spore restores ready to work:

```yaml
sandbox:
  warmup:
    - apk add --no-progress build-base git
    - go mod download
```

Each entry is a shell command run in the workspace. A failing warmup step
aborts the bake and destroys the builder.

## Mediation

Mediation declares the named credential services a baked spore may reach
through the lineage gateway. Secrets stay host-side; the guest sees a local
endpoint, never the credential:

```yaml
sandbox:
  mediation:
    services: [github-token]
```

The gateway operator grants services per lineage in the gateway's own config;
a spore gets the intersection of what its policy requests and what the
operator granted. See the [SporeVM layer plan](plans/sporevm-layer.md) for the
gateway design.

## Not Yet Translated

These policy fields still parse (so `policy validate` accepts them) but fail
`compile` until they map onto SporeVM:

- `sandbox.resources.disk`
- stage-scoped network policy (`repository.network`, per-stage allowlists)
- `sandbox.docker`
- `sandbox.dependencies` and `sandbox.services` blocks (use `warmup`)
- `sandbox.run.before`
