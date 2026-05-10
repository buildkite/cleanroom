# Policy

Cleanroom reads repository policy from `cleanroom.yaml`, then
`.buildkite/cleanroom.yaml`.

Policy is repo intent. It should describe the image, resources, network access,
Docker need, repository checkout, and create-time setup that CI expects. Host
runtime details belong in `~/.config/cleanroom/config.yaml`.

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

Use a digest-pinned image in committed policy:

```bash
cleanroom image resolve ghcr.io/buildkite/cleanroom-base/alpine:latest
cleanroom image bump-ref ghcr.io/buildkite/cleanroom-base/alpine:latest
```

## Repository Checkout

Top-level `cleanroom create`, `cleanroom exec`, and `cleanroom console` commands
are repo-aware when run inside a Git checkout. By default they resolve the
current `origin` remote, exact `HEAD`, and checkout path `/workspace`.

You usually do not need a `repository` block. Add one only to override defaults
or disable repo bootstrap:

```yaml
repository:
  remote: origin
  path: /workspace
  submodules: false
```

```yaml
repository:
  enabled: false
```

Use `--repo-url` for examples or CI-style runs outside the target checkout:

```bash
cleanroom exec --repo-url https://github.com/buildkite/agent.git -- go test ./...
```

See [Workspaces](workspaces.md) for local edits, copy-in, copy-out, and sync.

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

Rules are host and port rules, not URLs. DNS resolution is part of enforcement:
hostname rules are learned from DNS answers and enforced as destination
IP:port pairs.

Stage-scoped networking lets repository checkout, dependency setup, service
setup, and execution have different allowlists:

```yaml
repository:
  network:
    allow:
      - github.com:443

sandbox:
  network:
    default: deny
    dependencies:
      allow:
        - proxy.golang.org:443
        - sum.golang.org:443
    services:
      allow:
        - registry-1.docker.io:443
    execution: {}
```

Do not combine `sandbox.network.allow` with stage-local network blocks.
Stage-scoped egress is supported by `darwin-vz`; `firecracker` currently fails
closed for stage-scoped policies until that backend has active per-command
egress updates.

## Resources And Docker

```yaml
sandbox:
  resources:
    vcpus: 4
    memory: 8GiB
    disk: 16GiB
  docker:
    required: true
```

`docker.required: true` starts Docker inside the microVM for dependency,
service, and execution commands that need it.

## Dependency And Service Blocks

Dependency blocks run during sandbox creation and publish reusable setup outputs.
They are best for deterministic repo setup such as `npm ci`, `go mod download`,
`bundle install`, and toolchain installs.

```yaml
sandbox:
  dependencies:
    - name: node
      command: npm ci
      inputs:
        files: [package.json, package-lock.json]
      outputs:
        dirs: [node_modules]
```

Service blocks also run during sandbox creation. They are best for on-disk
service state that can be prepared once and reused, while live services start in
`sandbox.run.before` before each execution.

```yaml
sandbox:
  docker:
    required: true
  services:
    - name: database
      command: |
        docker compose up -d postgres valkey
        bin/rails db:prepare
        docker compose stop postgres valkey
      inputs:
        files: [docker-compose.yml, db/schema.rb]
      outputs:
        dirs: [/var/lib/docker]
  run:
    before: docker compose up -d postgres valkey
```

Block commands run in the repository workdir. `inputs.files` are
repository-relative files or globs. Output dirs and files can use relative
workspace paths, `${WORKSPACE}`, `${HOME}`, or absolute guest paths. Outputs
cannot be `/`, the repository root, overlapping, or glob patterns.

Use [Caching](caching.md) for the runtime behavior behind these blocks.
