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

## Warmup And Blocks

Warmup commands run once inside the builder VM during `cleanroom bake`, after
the workspace is copied in. Use them to install dependencies and warm caches
so the captured spore restores ready to work:

```yaml
sandbox:
  warmup:
    - apk add --no-progress build-base git
    - go mod download
```

Each explicit `warmup` entry is a shell command run in the workspace. A failing
warmup step aborts the bake and destroys the builder.

`dependencies` and `services` blocks are lowered into effective warmup steps.
The execution order is:

1. explicit `sandbox.warmup` entries, in declaration order;
2. `sandbox.dependencies` blocks, in declaration order;
3. `sandbox.services` blocks, in declaration order.

Explicit warmup runs first so policies can install toolchains before dependency
commands. Dependency blocks run before service blocks because service setup may
need those dependencies. Bake executes every effective step as
`cd /workspace && <step>`, so generated block commands do not change directory
themselves.

```yaml
sandbox:
  warmup:
    - apk add --no-progress go
  dependencies:
    reuse: exact # default; may also omit the object wrapper and declare a list
    blocks:
      - name: go-modules
        command: go mod download          # strings run as: sh -lc <script>
        inputs:
          files: [go.mod, go.sum]
        env:
          GOPROXY: https://proxy.golang.org,direct
        outputs:
          dirs: ["${HOME}/go/pkg/mod"]
  services:
    - name: cache
      command: [sh, -lc, bin/start-cache-service]
      inputs:
        files: [service-config.yml]
      outputs:
        dirs: [/var/lib/cleanroom/services/cache]
```

A block lowers to one shell step: sorted `env` assignments with shell-quoted
values, followed by the command with argv shell-quoted. String commands are
normalised to `sh -lc <script>`; sequence commands are treated as argv. Do not
put credentials in block `env`: values are part of the baked policy, appear in
warmup logs, and may be captured in the spore. Use mediation for credentials.

Block `inputs.files` and `outputs` are declaration metadata. Inputs are
validated and hash-covered as policy metadata; content freshness comes from the
commit/dirty bake key. Outputs are honoured by the whole checkpoint: everything
present in the VM when `spore save --out ... --stop` runs is captured, while output
path/overlap validation prevents ambiguous declarations.

`dependencies.reuse: exact` and the default are accepted because the whole
checkpoint captures block outputs and the commit/dirty bake key is conservative.
`dependencies.reuse: portable` fails `compile`; input-only portable block-cache
semantics are not implemented and would require a bake key/hash scheme change.

## Mediation

Mediation declares the named credential services a baked spore may reach
through the lineage gateway. Secrets stay host-side; the guest sees a local
endpoint, never the credential:

```yaml
sandbox:
  mediation:
    services: [github-token]
```

`content-cache` is also a mediation service. In the common content-cache-only
case, `cleanroom run` audits the spore's bake key against the current policy,
checks `127.0.0.1:8128/health`, and starts a child backing cache for the run if
one is not already available. That child is scoped to the audited policy's
allowed hosts, and the temporary gateway config grants the service to the
audited policy hash. `cleanroom run` also injects per-command Git and Go
environment such as Git `insteadOf`, `GOPROXY`, and `MISE_GO_DOWNLOAD_MIRROR`
values pointing at `/services/content-cache/...`. This setup happens at run
time; bake only stamps policy/provenance.

For prewarming, debugging, or managed host setup, run
`cleanroom content-cache serve`. It listens on `127.0.0.1:8128` by default and
keeps storage in the user's cache directory, so multiple spores from the same
repository can share the same host cache through separate per-run gateways.

The gateway operator grants services per lineage in the gateway's own config;
a spore gets the intersection of what its policy requests and what the
operator granted. See the [SporeVM layer plan](plans/sporevm-layer.md) for the
gateway design.

## Not Yet Translated

These policy fields still parse (so `policy validate` accepts them) but fail
`compile` until they map onto SporeVM:

- `sandbox.resources.disk`
- stage-scoped network policy (per-stage allowlists)
- `sandbox.docker.required` (docker-in-guest is deferred)
- `sandbox.dependencies.reuse: portable`
- `sandbox.run.before`

## Removed Fields

Top-level `repository:` and `expose:` blocks belonged to the old runtime and are no longer part of the policy schema. Strict parsing rejects policies that still declare them.
