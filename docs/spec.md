# Cleanroom Specification

## 1) Vision
Cleanroom provides repository-scoped sandbox profiles that define exactly what outbound network access and package registries a build/test run may use. A sandbox can be instantiated from these rules, enforcing deny-by-default egress while still allowing required external dependencies. Package registry traffic is routed through and filtered by a content cache layer (`https://github.com/wolfeidau/content-cache`) to improve hermeticity, repeatability, and policy enforcement.

Trust boundary for v1:
- The cleanroom creator (developer, CI, or trusted outer-ring agent) is trusted.
- Workload code executed inside the sandbox is untrusted.
- Security guarantees apply to enforcement inside the cleanroom boundary, not to review/approval workflows for policy changes.

Initial execution backend:
- **Local sandbox:** Firecracker microVM on Linux/KVM

## 2) Objectives
1. Repository-owned configuration defines all allowed network egress and registries.
2. Sandboxes enforce least-privilege network access with default deny.
3. Package fetches are only allowed via approved registries and through `content-cache` filtering.
4. Backends are pluggable so remote and local runtimes can be swapped without changing policy schema.
5. Secret usage is explicit, short-lived, and never embedded in repository files or guest process env.
6. Policy and runtime events are auditable for security review and incident response.

## 3) Scope
### In scope
- Policy schema, validation, and policy-driven sandbox creation.
- Sandbox backend with a common adapter interface (Firecracker on Linux/KVM for v1).
- Host-level network allowlisting + package-registry mediation via content-cache.
- Client/server control plane with local CLI client for creating and managing sandboxes and executions.
- Logs, metrics, and audit metadata capture.
- Backend capability declaration and fail-closed launch checks.

### Out of scope (v1)
- Full kernel-level deep packet inspection / DLP.
- Per-command inline prompt-level policy decisions.
- Remote execution backends and multi-cloud scheduler federation.

### 3.1 Responsibility split (v1)
Cleanroom specifies the normative security contract for a sandbox and its executions:
- Policy semantics and match behavior (`allow`/`deny`, precedence, defaults).
- Runtime invariants (policy load timing, immutability during sandbox lifetime).
- Enforcement outcomes (what must be blocked/allowed for a given effective policy).
- Required audit event schema and deny reason codes.
- Backend capability requirements and fail-closed behavior.

Backend adapters are implementation-specific and may differ in mechanism:
- Packet filtering, DNS wiring, VM/runtime internals, and process isolation primitives.
- Log transport and collection path.
- Secret delivery mechanism internals, as long as Cleanroom invariants are met.

Deferred past MVP:
- Organization-wide inherited/baseline policy layering.
- Advanced destination identity controls (for example cert pinning).
- Advanced DLP and deep traffic inspection.
- Full multi-ecosystem lockfile/parser parity on day one.

## 4) User stories
- As a maintainer, I can check in a policy file and ensure every build run can access only approved hosts.
- As a developer, I can run a command that executes my existing test commands inside a compliant sandbox.
- As a security reviewer, I can see exactly which hosts and registries were allowed for each sandbox execution.
- As an SRE, I can configure backend selection and runtime options independently of repository policy.
- As a developer, I can preinstall repository-defined toolchains (for example via `mise`) with an explicit dependency bootstrap command inside the sandbox.
- As a developer, I can explicitly package local uncommitted repository changes into a reproducible changeset and run that inside a sandbox without mounting my host workspace.

## 5) Policy model
### 5.1 Repository config
Repository policy file resolution (in order):
1. `cleanroom.yaml` in repository root
2. `.buildkite/cleanroom.yaml` (fallback path)

If both exist, root `cleanroom.yaml` is authoritative and `.buildkite/cleanroom.yaml` is ignored with a warning.

```yaml
version: 1

repository:
  enabled: true
  remote: origin
  path: /workspace
  submodules: false
  network:
    allow: github.com:443

sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/debian@sha256:28c3f638fabe1ed780f87b82cfb0c6dda2549c86b9e4edbe519e8250243411c5
  resources:
    vcpus: 4
    memory: 8GiB
    disk: 16GiB
  docker:
    required: true
  dependencies:
    - name: toolchains
      command: mise install
      inputs:
        files:
          - .mise.toml
      outputs:
        dirs:
          - ${HOME}/.cache/mise
          - ${HOME}/.local/share/mise
    - name: go-modules
      command: mise exec -- go mod download
      inputs:
        files:
          - go.mod
          - go.sum
      env:
        GOMODCACHE: ${HOME}/go/pkg/mod
      outputs:
        dirs:
          - ${HOME}/go/pkg/mod
  run:
    before: mise exec -- go test ./...
  network:
    default: deny
    dependencies:
      allow:
        - proxy.golang.org:443
        - sum.golang.org:443
    services:
      allow: ghcr.io:443
    execution: {}
```

### 5.1.1 Repository bootstrap config

Top-level commands default to materializing the current git repository into the
sandbox when run inside a git checkout.

The implicit defaults are:

```yaml
repository:
  remote: origin
  path: /workspace
  submodules: false
```

The optional `repository` block is for overrides or disablement:

```yaml
repository:
  enabled: false
```

Meaning:

- `enabled: false` disables the default repo-aware bootstrap for top-level commands
- `remote` selects which git remote to read, default `origin`
- `path` is the absolute guest destination, default `/workspace`
- `submodules` controls whether submodules are initialized after checkout

Repository policy controls only the exact committed checkout behavior. It does
not include a field that implicitly copies or mounts local dirty-worktree
content into the sandbox. Any inclusion of local modifications must use an
explicit request-time changeset input.

### 5.2 Schema rules
- Required: `version`, `sandbox.image.ref`.
- `sandbox.image.ref` must be a digest-pinned OCI image reference. CLI image
  override paths may resolve tags or local images before building the compiled
  policy, but repository policy files should be pinned.
- `sandbox.network.default` defaults to `deny` when omitted. Repository policy
  files currently accept only `deny`; `allow` is reserved for compiled policies
  produced by CLI `--dangerously-allow-all` paths when creating a new sandbox
  and is not valid in `cleanroom.yaml`.
- If `repository` is omitted, top-level commands default to the current repo with `remote: origin`, `path: /workspace`, and `submodules: false`.
- `repository.enabled` defaults to `true`; `false` disables repo-aware bootstrap for top-level commands.
- `repository.mode` is optional and, when set, may be `none` or `current-repo`.
- `repository.remote` defaults to `origin`.
- `repository.path` defaults to `/workspace` and must be an absolute guest path.
- `repository.submodules` defaults to `false`.
- `sandbox.resources` defaults to unset. When present, it declares backend-neutral minimum workload requirements, not exact host reservations.
- `sandbox.resources.vcpus` must be a positive integer.
- `sandbox.resources.memory` and `sandbox.resources.disk` must be positive byte sizes. They accept raw bytes or size strings such as `4096MiB`, `8GiB`, or `16GiB`.
- Resource floors raise the effective backend runtime settings when the host runtime config is lower; they do not lower larger host defaults.
- Backends decide how effective resource ceilings map to host resources.
- Sandbox API responses include `effective_resources`, which reports the resolved vCPU, memory, and disk ceilings after backend config, defaults, and policy resource floors are merged.
- `effective_resources.vcpus` is a launch-time CPU ceiling; Cleanroom does not hotplug CPUs into an already running sandbox.
- `sandbox.docker.required` defaults to `false`; when true, Cleanroom starts the guest Docker daemon for the sandbox.
- `sandbox.dependencies` defaults to an empty block list; each block has `name`, `command`, `inputs.files`, and `outputs.dirs` or `outputs.files`. It may be a block list directly, or an object with `reuse` and `blocks` when dependency-stage options are needed.
- `sandbox.dependencies.reuse` may be `portable` when `sandbox.dependencies.blocks` declares at least one input file; omitted or `exact` means exact checkout reuse only.
- Dependency block commands run in the repository workdir during sandbox creation and make declared outputs eligible for dependency-stage caching.
- Dependency block commands accept either a YAML string or a YAML sequence; strings execute as `sh -lc <value>`, and strings are the preferred form.
- `sandbox.services` defaults to an empty block list; each block has `name`, `command`, `inputs.files`, and `outputs.dirs` or `outputs.files`.
- Service block commands run in the repository workdir after dependency bootstrap during sandbox creation and make declared outputs eligible for services-stage caching.
- Service block commands accept either a YAML string or a YAML sequence; strings execute as `sh -lc <value>`, and strings are the preferred form.
- Block `inputs.files` are repository-relative regular files or globs matching
  regular files. Literal paths must exist; globs must match at least one path.
- Block `outputs.dirs` and `outputs.files` are guest paths written by the block.
  Relative paths are repository-relative and resolve against `repository.path`;
  for example `node_modules` resolves to `/workspace/node_modules` with the
  default repository path. Absolute paths are also valid. Outputs cannot be `/`
  or the repository root, must stay within the repository root when declared
  with a relative path or `$WORKSPACE` prefix, and must not contain glob
  characters. `${HOME}`, `$HOME`, and `~` expand to the Cleanroom block
  execution home, while `${WORKSPACE}` and `$WORKSPACE` expand to the normalized
  repository path.
- Directory outputs may already exist in the repository, such as
  `public/assets/.keep`. On a cache miss, Cleanroom seeds the empty output
  volume from that directory before the block runs. On a cache hit, the cached
  output volume is authoritative and is mounted over the checkout directory.
  Seed files do not need to be listed separately in `inputs.files` just to be
  copied into the output volume; seed changes are covered by the repository and
  changeset identity used in the block cache key.
- Block `env` values are literal strings, except leading home-directory
  and workspace shorthand forms (`~`, `~/`, `$HOME`, `$HOME/`, `${HOME}`,
  `${HOME}/`, `$WORKSPACE`, `$WORKSPACE/`, `${WORKSPACE}`, `${WORKSPACE}/`)
  expand to the corresponding Cleanroom guest paths.
- Normalized outputs must be unique and non-overlapping across all dependency
  and service blocks.
- `sandbox.run.before` defaults to unset; when present, each execution runs that shell command in the repository workdir immediately before the requested command.
- `sandbox.run.before` must be a YAML string or block string and executes as `sh -lc <value>`.
- Policy schema intentionally has no field for implicit dirty-worktree inclusion; explicit local modifications are a separate request-time changeset input.
- `sandbox.network.allow` defaults to empty. It accepts either a sequence of
  entries or a single scalar entry.
- `repository.network.allow` is the workspace-stage allowlist for repository
  checkout and explicit repository changeset application.
- `sandbox.network.dependencies.allow` applies only while dependency blocks run.
- `sandbox.network.services.allow` applies only while service blocks run.
- `sandbox.network.execution.allow` applies to `sandbox.run.before` and the
  requested execution command.
- Stage-local network blocks do not inherit from one another. An omitted
  stage-local block means no external egress for that stage when any stage-local
  network block is configured.
- `sandbox.network.allow` cannot be combined with stage-local network blocks. It
  remains the legacy all-stage allowlist when no stage-local network block is
  configured.
- An allow entry may be a mapping with an exact `host` value and at least one
  explicit port in `ports`, or the scalar shorthand `host:port`.
- The `host:port` shorthand must include one explicit port from 1 to 65535.
  Bare hosts, URLs, paths, port ranges, and IPv6 literals are not accepted in
  shorthand form. Use the mapping form for hosts that cannot be represented
  unambiguously as `host:port`.
- Host matching is exact after trimming whitespace and lowercasing the policy
  and requested host values. Wildcards and CIDR ranges are not part of the
  current policy-file schema.
- Ports are required and must be integers from 1 to 65535.
- The current policy-file schema has no `sandbox.network.deny`, `registries`,
  `metadata`, `secrets`, or lockfile-enforcement fields. Those concepts may be
  added later, but unknown fields intentionally fail validation today.

### 5.2.1 Deterministic network match semantics (normative)
- Cleanroom trims whitespace and lowercases policy hosts and requested
  destination hosts before matching.
- A destination is allowed only when an exact host and port entry matches the
  effective policy for the active stage.
- The active stages are `workspace` for repository checkout and changeset
  application, `dependencies` for dependency bootstrap, `services` for services
  bootstrap, and `execution` for `sandbox.run.before` plus the requested
  command.
- If no stage-local network block is configured, the legacy
  `sandbox.network.allow` list is the effective policy for every stage.
- Any destination not matched by an exact allow entry is denied.
- IP literals are treated as exact host values. CIDR matching is not currently
  implemented.
- Repository policy files cannot request allow-by-default behavior. The only
  allow-by-default mode is the internal compiled policy produced by
  `--dangerously-allow-all` on new-sandbox CLI paths, including repo-aware
  top-level commands and repo-agnostic `cleanroom sandbox create`.

### 5.2.2 Future schema ideas (non-normative)

The following ideas are intentionally not accepted in `cleanroom.yaml` today:

- wildcard host rules, with explicit single-label versus multi-label matching
  semantics
- CIDR rules for IP-literal destinations
- optional port defaults such as `{80,443}`
- explicit deny rules, if Cleanroom later supports allow-by-default policy files
- first-class registry or package-manager policy blocks
- lockfile-derived artifact allowlists
- opaque policy metadata annotations

### 5.3 Future: secret references (non-normative)

Secret binding policy is not part of the current repository policy schema. A
future implementation may allow policy files to reference secret identifiers,
but not plaintext token values. Secret IDs would be resolved at run time from
the CI environment or an external secret provider.

A future runtime policy object may contain:
  - `secret_id` (e.g. `npm_readonly`, `github_pat_ci`)
  - `target` (which backend uses it: content-cache, secret-proxy, or direct env-injected)
  - `allowed_hosts` (host restrictions for each secret binding)
  - `ttl_seconds` and optional `single_use` hints.
- Secret material is provisioned only to the cleanroom control process and never mounted into the guest filesystem.

### 5.4 Execution model
- A sandbox is the primary unit of lifecycle. It is created once and accepts multiple executions.
- `cleanroom exec` is the primary command for running arbitrary commands in a sandbox.
- `cleanroom exec` uses a `--` command separator consistent with common shell tooling.
- Unless an explicit vector command form is added later, `cleanroom exec` defaults to shell execution (e.g., `/bin/sh -lc`) so commands like `cleanroom exec "npm test"` work directly.
- Cleanroom uses a client/server architecture:
  - `cleanroom` CLI resolves and compiles policy from repository files.
  - `cleanroom serve` validates compiled policy and manages sandbox and execution lifecycle via backend adapters.
  - all CLI commands, including `cleanroom exec`, call the server API.
  - "local execution" means local backend selected by the server, not a direct non-API code path.
- Filesystem writes persist across executions within a sandbox and are discarded on sandbox termination.
- Current API/runtime intent: no host workspace mount input is accepted by `CreateSandbox` or `CreateExecution`.
- Explicit local modifications, when requested, are packaged as a changeset input and applied after exact repository checkout rather than mounted from the host.
- Workloads run against the backend-provided sandbox image filesystem.
- By default, `cleanroom create`, `cleanroom exec`, and `cleanroom console` are repo-aware when run inside a git repository:
  - they resolve the local repository remote URL and committed `HEAD`
  - they materialize that checkout inside the sandbox before the command runs
  - they start commands in `repository.path`
  - when `sandbox.dependencies` blocks are set, sandbox creation runs those dependency bootstrap blocks after repository bootstrap and may publish a reusable dependency stage for later warm hits
  - when `sandbox.services` blocks are set, sandbox creation runs those services bootstrap blocks after dependency bootstrap and may publish a reusable services stage for later warm hits
  - when `sandbox.run.before` is set, each execution runs that shell command immediately before the requested command
  - Cleanroom does not auto-detect or auto-wrap `mise`; use explicit commands such as `mise exec -- ...` when needed
- When explicitly requested, top-level commands may package local modifications against committed `HEAD` into a reproducible changeset, send that changeset as a separate sandbox-creation input, and apply it after repository bootstrap and before dependency bootstrap, services bootstrap, or workload execution.
- `cleanroom sandbox create` remains the generic low-level surface and does not
  infer repository state from the current working tree or read `cleanroom.yaml`.
- Without `--from`, `cleanroom sandbox create` synthesizes a repo-agnostic
  policy using the selected image ref, deny-by-default networking by default,
  optional guest Docker service enablement via `--docker`, and optional
  unrestricted outbound networking via `--dangerously-allow-all`.
- With `--from <snapshot-id>`, `cleanroom sandbox create` restores a sandbox
  from the snapshot and does not accept create-time policy override flags such
  as `--image`, `--docker`, or `--dangerously-allow-all`.

### 5.4.1 `cleanroom exec` behavior contract (normative)
- `cleanroom exec` must:
  1. resolve API endpoint (`--host`, env, context, default unix socket),
  2. resolve and compile policy,
  3. when repo-aware bootstrap is enabled, resolve the current git repository
     root, remote URL, and committed `HEAD`,
  4. when explicit local-changes mode is enabled, package a reproducible
     changeset against that committed `HEAD`,
  5. create or select sandbox,
  6. create execution,
  7. stream output/events to caller,
  8. return workload exit code.
- Default mode creates an ephemeral sandbox, runs the command, and terminates
  the sandbox afterward.
- `--keep` preserves a newly created sandbox after execution completes.
- Reuse an existing sandbox with `--in <id>`.
- Create a new sandbox from a snapshot with `--from <snapshot-id>`.
- `cleanroom exec` defaults to non-interactive server-streaming semantics.
- `cleanroom exec --tty` and `cleanroom console` must use `AttachExecution`
  bootstrap plus the dedicated QUIC interactive transport.
- First interrupt signal should request execution cancel; second interrupt may detach client stream immediately.
- If the local repository is dirty and no explicit changeset mode is requested,
  Cleanroom should warn and continue using committed `HEAD`; uncommitted changes
  must not be copied into the sandbox.

### 5.4.2 Explicit local changesets (normative)

Local modifications must remain an explicit opt-in path rather than an implicit
extension of `current-repo`.

Requirements:

- The exact committed checkout remains the default repository input.
- Dirty-worktree inclusion must be triggered only by an explicit request-time
  changeset mode.
- A changeset must be bound to a concrete base checkout:
  - canonical remote URL
  - full base commit SHA
  - submodule mode or equivalent repository materialization inputs
- A changeset must carry deterministic identity metadata:
  - a versioned transport format identifier
  - a stable changeset digest
  - the expected post-apply tree digest
- The client may create the changeset from local repository state, but the
  control plane must validate that the base checkout in the request matches the
  changeset metadata before applying it.
- When a host-local changeset store is available, the control plane should
  persist the validated changeset identity and replay payload before applying
  it so cache lineage can refer to a stable changeset ID.
- The control plane must apply a changeset only after exact repository checkout
  or workspace-stage restore and before dependency bootstrap or workload
  execution.
- If changeset application fails, or if the resulting repository tree digest
  does not match the expected post-apply tree digest, sandbox creation must fail
  closed.
- Dependency and service input resolution must use the post-apply repository tree
  when a changeset is present.
- File-keyed dependency and service output reuse requires declared block inputs
  and outputs and must validate declared inputs before publishing reusable
  outputs.
- Changesets are separate from both user snapshots and system-managed stage
  caches. They are explicit request inputs, not snapshot restore targets.

Transport format is intentionally implementation-defined in this spec, but it
must be deterministic, versioned, and replayable without mounting the host
workspace. A Git-native bundle or equivalent canonical archive is acceptable for
the first implementation slice.

### 5.5 Compiled policy payload (normative)
Cleanroom compiles repository policy into an immutable `CompiledPolicy` payload for sandbox creation. That payload is then reused for every execution in the sandbox. It is the only policy input to backend adapters.

Minimum required fields:
- `version`
- `image_ref`
- `image_digest`
- `network_default`
- `allow[]`
  - `host`
  - `ports[]`
- `network_stages`
  - optional `workspace`, `dependencies`, `services`, and `execution`
    allowlists
- `services`
  - optional service blocks with command, inputs, and outputs
- `dependencies`
  - optional dependency blocks with command, inputs, and outputs
- `docker`
  - Docker service requirement
- `run`
  - optional pre-run command
- `resources`
  - optional minimum `vcpus`, `memory_bytes`, and `disk_bytes` resource floors
- `hash` (digest of the compiled policy payload)

Requirements:
- Backend adapters must not re-resolve policy from repository files.
- Runtime behavior is derived only from `CompiledPolicy`.
- Explicit local changesets are separate request inputs and must not be folded
  into `CompiledPolicy` or its hash.

## 6) Runtime behavior
### 6.1 Launch flow
All runtime launch behavior is initiated by control-plane API calls (for example `CreateSandbox` and `CreateExecution`) from CLI or SDK clients.

1. CLI/SDK client resolves spec file using precedence above.
2. CLI/SDK client compiles policy and sends it in `CreateSandbox`.
3. Control plane validates compiled policy and backend selection.
4. Start sandbox via selected backend.
5. Register sandbox identity in host gateway and install anti-spoof + gateway reachability rules.
6. Enforce runtime policy:
  - DNS/egress allowlist only.
  - outbound packet filtering to allowed host:port/protocol.
7. Run workload command.
8. Emit structured events + exit status. Sandbox remains `READY` for further executions until explicitly terminated or TTL expires.

### 6.1.1 Policy load and immutability
- Policy is loaded and compiled by the client, then provided to the control plane at sandbox creation.
- Backend adapters receive a compiled immutable policy payload; they must not receive repository file paths for policy re-resolution.
- Active sandboxes do not support runtime policy mutation.
- Backend adapters must not re-read repository policy files after sandbox creation.
- Guest workloads cannot mutate control-plane policy inputs for the active sandbox.

### 6.2 Host gateway

Cleanroom runs a single shared host gateway process that provides mediated access to external services for sandboxes on the host. The current implementation serves Git, OCI registry, Go module proxy, RubyGems, and immutable fetch routes today, while keeping secret and metadata routes reserved for follow-up work. Sandbox identity is derived from the network transport layer, not bearer tokens.

#### 6.2.1 Transport and sandbox identity

The gateway uses TAP-network TCP rather than `AF_VSOCK` for guest-to-host service access. In nested virtualisation environments (for example Firecracker inside EC2), the outer hypervisor owns the vsock device and CID namespace. Host-side vsock listener binds (`CID=2` / `CID=0`) fail with `cannot assign requested address` because the host kernel's `vhost_vsock` driver cannot simultaneously act as a vsock guest to the outer hypervisor and a vsock host to inner VMs. The reverse direction (host dials guest) works because Firecracker mediates it through its own vsock UDS path, which is why the guest exec agent continues to use vsock. The TAP network has no such constraint — it is managed entirely within the host kernel's networking stack.

Each sandbox is provisioned with a unique TAP device and a unique guest IP. The host gateway identifies the calling sandbox by mapping the source IP of incoming connections to the registered sandbox and its `CompiledPolicy`.

- The gateway listens on a small fixed set of host ports (or a single port with path-prefix routing for `/git/`, `/registry/`, `/goproxy/`, `/rubygems/`, `/fetch/`, `/secrets/`, `/meta/`).
- Sandbox identity is resolved from the connection source IP. No tokens, API keys, or path-embedded credentials are used for sandbox identification.
- The gateway maintains a registry of `(guestIP → sandboxID → CompiledPolicy)` populated at sandbox creation and removed at teardown.
- The guest discovers the gateway via its host-side TAP IP on well-known ports, injected into the command environment (for example `GIT_CONFIG_GLOBAL`, `HTTP_PROXY`, or equivalent per-service environment variables).

This model scales to O(services) listening ports regardless of sandbox count, avoiding port exhaustion at high density.

#### 6.2.2 Anti-spoofing and reachability (normative)

Source-IP identity requires explicit anti-spoofing enforcement on each TAP interface:

- For each sandbox TAP `tapX` with guest IP `GUEST_IP`, drop any packet arriving on `tapX` not sourced from `GUEST_IP`.
- Disable IPv6 on sandbox TAP interfaces (or install equivalent v6 anti-spoof rules).
- Gateway ports must only be reachable from sandbox TAP interfaces. Host INPUT rules must reject gateway-port traffic from non-TAP sources (host LAN, Docker bridges, and similar).
- Per-sandbox INPUT rules may further restrict which gateway service paths are reachable based on compiled policy (for example a sandbox whose policy includes no secret bindings should not reach the secrets endpoint).

These rules are installed during sandbox network setup and torn down during cleanup, following the same lifecycle as existing FORWARD rules.

#### 6.2.3 Git proxy

- Cleanroom rewrites clone URLs to the host gateway's git endpoint when a repo host is in the effective policy for the active stage.
- Runtime injects scoped Git URL rewrite config inside the sandbox command environment (for example `url.<gateway>/git/<host>/.insteadOf=https://<host>/`).
- Clone commands run unchanged inside the sandbox (`git clone https://github.com/org/repo.git`), with transport rewritten to the gateway endpoint.
- The gateway resolves the target host from the request path, validates it against the sandbox's active effective policy, and proxies the git smart-HTTP protocol (`info/refs?service=git-upload-pack` and `git-upload-pack`) upstream.
- Upstream authentication is held host-side by the gateway process (for example GitHub App installation tokens or PATs resolved from the CI secret store). Credentials are never exposed to the guest.
- Enforcement:
  - deny by default except policy-allowed git hosts.
  - credential injection scoped by upstream host prefix.
  - `.git` smart-HTTP routes are served through embedded `content-cache`, with Cleanroom policy enforcement wrapped around the cache handler.
  - gateway may later add offline fallback for warm entries.

#### 6.2.4 Package registry proxy

- The gateway exposes a `/registry/` route backed by embedded `content-cache` OCI handlers.
- The gateway resolves a registry prefix from the request path, maps it to an upstream registry URL, and applies the sandbox allowlist against the mapped policy host and port before forwarding upstream. Registry redirects are also policy-checked against the redirected host and port.
- Current route scope is OCI pull-style `GET` and `HEAD` traffic.
- The gateway also exposes a Docker Hub-compatible `/v2/` mirror endpoint backed by the same OCI cache so guest `dockerd` can use the shared gateway as a Docker Hub registry mirror.
- Guest `dockerd` also gets generated Docker registry-host config for built-in public registries (`ghcr.io`, `public.ecr.aws`) and non-Docker-Hub registries explicitly configured in runtime config under `gateway.oci.registries`, pointing those registry namespaces' server endpoint at the gateway `/registry/<host>/` path without a direct upstream fallback.
- Current Docker mirror scope is pull-style `GET` and `HEAD` traffic for Docker Hub, built-in public registry hosts, and runtime-configured registry hosts.
- The gateway exposes a `/goproxy/` route backed by embedded `content-cache` Go module proxy handlers, including mirrored sumdb traffic under `/goproxy/sumdb/`.
- Current Go module scope includes `GOPROXY` metadata requests, module zip downloads, and mirrored checksum-database requests.
- The gateway also exposes a `/rubygems/` route backed by embedded `content-cache` RubyGems handlers for Bundler mirror traffic to `rubygems.org`.
- Current RubyGems scope includes Compact Index metadata, legacy specs metadata, and gem downloads.
- The gateway exposes a `/fetch/` route backed by embedded `content-cache` immutable artifact handlers for configured hosts such as `dl.google.com`.
- Unsupported registry requests are denied with explicit audit reason (`registry_not_allowed`) or route-specific validation errors.
- Future work:
  - guest-side package-manager rewrites through `/registry/`
  - broader guest-side tool download rewrites beyond the current `/fetch/` slice
  - broader guest-side RubyGems source rewrites beyond the default Bundler mirror path
  - lockfile enforcement
  - broader non-OCI package-manager protocol support
  - optional metadata signing/validation hooks

#### 6.2.5 Future: secret injection

The `/secrets/` gateway route and policy-level secret bindings are reserved for
future work. A future implementation may inject credentials on the upstream leg
without exposing them to the sandbox, validate each credential against
host-scoped bindings, and log secret IDs without logging secret values.

#### 6.2.6 Audit attribution

- Every gateway request is inherently attributed to a sandbox ID from the transport identity (source IP), independent of any guest-supplied metadata.
- The gateway emits structured audit events per request with `sandbox_id`, `service` (git/registry/secrets/meta), `destination_host`, `action` (allow/deny), and `reason_code`.
- Deny events use the stable reason codes defined in section 9.1.

#### 6.2.7 SSRF and cross-service hardening

- The gateway must never act as a general-purpose HTTP proxy. It must only connect upstream to destinations explicitly permitted by the requesting sandbox's compiled policy.
- Request path canonicalisation must prevent traversal between service endpoints (for example `/git/../../secrets/`).
- Upstream connection pools must not be shared across sandbox identities to prevent request smuggling across sandbox boundaries.
- The gateway must validate `Host` headers and request paths before routing to prevent host confusion attacks.

### 6.3 Default-fail semantics
- Any destination not matched by explicit allowlist is denied.
- Gateway-backed registry, package, and fetch routes must still pass the
  sandbox policy allow check for their upstream host and port before forwarding.
- Failed policy validation blocks launch.

### 6.4 Future: lockfile-restricted package fetches

Lockfile-derived artifact policy is not part of the current repository policy
schema. A future implementation may add package-manager-specific policy that:

- parses lockfiles during policy compilation
- builds an explicit artifact allowlist for each supported package manager:
  - package identity + version
  - registry endpoint
  - optional integrity/hash requirement
- passes those allow rules to the host gateway so it can only forward matching
  requests
- handles requests that miss lockfile constraints using an explicit mode:
  - `mode=deny_unknown`: block and emit `lockfile_violation` event.
  - `mode=warn`: allow but emit warning/metric.

Lockfile-derived restrictions would be an additional layer on top of
network/host allowlists, never a replacement.

## 7) Backend abstraction
Cleanroom provides a Go adapter interface for backend implementations:

- `Adapter`
  - `Name() string`
  - `ProvisionSandbox(ctx, ProvisionRequest) error`
  - `RunInSandbox(ctx, ExecutionRequest, OutputStream) (*ExecutionResult, error)`
  - `TerminateSandbox(ctx, sandboxID) error`
- `SnapshottingAdapter` (extends `Adapter`)
  - `CreateSnapshot(ctx, SnapshotRequest) (*SnapshotResult, error)`
  - `ProvisionSandboxFromSnapshot(ctx, ProvisionFromSnapshotRequest) error`
  - `DeleteSnapshot(ctx, DeleteSnapshotRequest) error`

### 7.1 Backend capability contract (required for launch)
Each backend must publish a capability map consumed by launch-time validation. Capabilities describe enforcement outcomes, not implementation details.

Current capability keys:
- `network.default_deny`: backend can enforce deny-by-default outbound network behavior.
- `network.allowlist_egress`: backend can enforce allowlist egress outcomes from compiled policy.
- `network.stage_scoped_egress`: backend can swap to the active stage allowlist
  before each sandbox command and revoke broader-stage connections.
- `dns_control_or_equivalent`: backend can prevent bypass of policy via unmanaged resolver paths.
- `network.guest_interface`: backend provides a managed guest network interface.

Optional capabilities (may be added by policy requirements later):
- `protocol_granularity`: protocol-specific network controls beyond baseline host/port behavior.
- `advanced_destination_identity`: stronger destination identity checks beyond hostname matching.
- `offline_cache_mode`: enforce no-upstream behavior when only warm cache artifacts are allowed.
- `secret_isolation`: backend can satisfy future secret exposure constraints.

Fail-closed rule:
- Launch must fail with `backend_capability_mismatch` when effective policy requirements exceed backend-declared capabilities.
- Cleanroom must not silently downgrade enforcement semantics when a backend lacks required capability.

### 7.2 Capability handshake format (normative)
Backends must expose capabilities in a machine-readable structure returned during adapter initialization and health checks.

Minimum shape:
- `backend_name`
- `backend_version`
- `capabilities`
  - capability key -> boolean
- `notes` (optional freeform diagnostics)

Policy feature mapping:
- Each policy compiler output feature must map to one or more required capability keys.
- Launch validation evaluates `CompiledPolicy` requirements against declared capabilities before provisioning.
- Any unmet requirement results in `backend_capability_mismatch`.

#### Local backend (firecracker)
- Firecracker microVM with per-execution TAP networking and iptables enforcement.
- Primary use: developer workflows and lightweight local CI.
- Controls at runtime via host iptables rules + managed DNS from compiled policy.

## 8) Configuration and integration points
- CLI command set (v1):
  - `cleanroom serve`
  - `cleanroom policy validate`
  - `cleanroom exec [--] <command>`
  - `cleanroom console [--] <command>`
  - `cleanroom inspect <typeid>`
  - `cleanroom sandbox inspect <sandbox-id>`
  - `cleanroom sandbox inspect --last`
  - `cleanroom sandbox ls`
  - `cleanroom sandbox rm <sandbox-id>`
  - `cleanroom execution ls`
  - `cleanroom execution inspect <execution-id>`
  - `cleanroom execution inspect --last`
  - `cleanroom execution inspect --sandbox-id <sandbox-id> --last`
  - `cleanroom doctor`
  - `cleanroom status [--execution-id <execution-id>|--last]`
  - `cleanroom image pull|ls|rm|import|bump-ref`
- CI integration:
  - `cleanroom exec --` wrapper for existing and local automation
  - machine-readable output (`--json`) for pipeline tooling
- API/SDK (v1):
  - ConnectRPC `SandboxService` (`CreateSandbox`, `GetSandbox`, `ListSandboxes`, `StatSandboxPath`, `WalkSandboxTree`, `ReadSandboxFile`, `WriteSandboxFile`, `RemoveSandboxPath`, `ArchiveSandboxPaths`, `ExtractSandboxArchive`, `TerminateSandbox`, `StreamSandboxEvents`)
  - ConnectRPC `ExecutionService` (`ListExecutions`, `CreateExecution`, `AttachExecution`, `GetExecution`, `InspectExecution`, `CancelExecution`, `WriteExecutionStdin`, `CloseExecutionStdin`, `StreamExecution`)

### 8.1 CLI and API failure contract (normative)
CLI:
- Validation failures (`cleanroom policy validate`, pre-launch compile errors) return non-zero and print structured error details.
- Launch failures (including `backend_capability_mismatch`) return non-zero before workload execution starts.
- Runtime policy denies do not change process semantics unless deny prevents command completion; deny events must still be emitted.
- Non-zero `cleanroom exec` and `cleanroom console` exits preserve streamed stdout/stderr and do not append automatic diagnostic footers.
- `cleanroom exec --print-sandbox-id` and `cleanroom exec --print-trace-id` remain the explicit opt-in surfaces for correlation identifiers.

API:
- `SandboxService.CreateSandbox` returns client error for invalid policy input and conflict/error response for unsatisfied backend requirements.
- `SandboxService.GetSandbox` must expose terminal sandbox status plus `last_execution_id` and `active_execution_id`.
- `ExecutionService.ListExecutions` must list active executions by default, include finished executions when explicitly requested, and return newest executions first.
- `ExecutionService.GetExecution` must expose execution status and exit code.
- `ExecutionService.InspectExecution` must expose richer diagnostics including message, retained stdout/stderr, artifacts location, `trace_id`, optional `trace_url`, and observability payload when available.
- ConnectRPC errors must include stable application `code` and human-readable `message`.
- `ExecutionService.StreamExecution` and interactive sessions bootstrapped by `ExecutionService.AttachExecution` must terminate cleanly with final exit status signaling.
- If an HTTP/JSON gateway is exposed, it must preserve the same stable error codes and reason semantics.

## 9) Audit and observability
- Emit structured logs per sandbox with:
  - `execution_id`
  - `actor`
  - `backend`
  - `timestamp`
  - policy digest (`policy_hash`) of the compiled effective policy payload
  - policy version hash
  - effective allowlist summary
  - backend, command, user/actor
  - blocked connection attempts (host, reason, timestamp)
- Deny events must use stable reason codes (for example `host_not_allowed`,
  `method_not_allowed`, `invalid_request`, `upstream_error`, or
  `unknown_registry_prefix`).
- Trace export configuration stays in runtime config and uses OTLP, with
  optional direct trace URL rendering from a configured template.
- Metrics:
  - launch success/fail by backend
  - rule violation counts
  - cache hit/miss latency and error rates
- Longitudinal audit retention: at least 90 days minimum.

### 9.1 Stable API error and gateway reason codes
Cleanroom uses stable API error codes for CLI and SDK error handling, and stable
gateway reason codes for gateway request audit, trace, and metrics labels.

Current API/client error classifiers:
- `unknown`
- `canceled`
- `deadline_exceeded`
- `invalid_argument`
- `not_found`
- `unavailable`
- `failed_precondition`
- `internal`
- `policy_invalid`
- `policy_conflict`
- `backend_unavailable`
- `backend_capability_mismatch`
- `host_not_allowed`
- `registry_not_allowed`
- `lockfile_violation`
- `secret_scope_violation`
- `runtime_launch_failed`

Current gateway request reason codes:
- `host_not_allowed`
- `method_not_allowed`
- `invalid_request`
- `upstream_error`
- `unknown_registry_prefix`
- `rubygems_unavailable`
- `proxied`
- `mirrored`
- `cached`
- `fallback`

Some API/client classifiers, such as `lockfile_violation` and
`secret_scope_violation`, are reserved for planned policy features but remain
stable client-facing codes.

## 10) Security considerations
- Principle of least privilege:
  - deny-by-default network, explicit allow rules only.
- Tamper-evident policy changes:
  - policy file changes require review before merge.
- Supply chain safety:
  - content-cache as first stop for registry access.
  - support offline modes where only pre-warmed cached artifacts are permitted.
- Future secret safety:
  - no plaintext secrets in policy, command args, or guest env.
  - secret IDs should be validated against policy and projected as short-lived bindings only.
  - all injection events should be logged with secret ID, destination, and reason, never with secret values.

## 11) Risks and open decisions
- Whether tokenizer-style injection runs as embedded process in cleanroom binary or as isolated helper.
- Hostname matching behavior under SNI/TLS certificates and proxy chains.
- Whether to allow dynamic hostlist generation (e.g., from lockfiles).
- Policy precedence model for parent/child directories and monorepo overrides.
- Whether all targeted ecosystems have reliable lockfile parser coverage and how to handle malformed locks.

## 12) Build plan
### Phase 1 (MVP)
- Spec schema + validator (`cleanroom.yaml` parser with `.buildkite/cleanroom.yaml` fallback)
- Core policy compiler to normalized exact-host allowlist
- Local backend (Firecracker) implementation
- embed `content-cache` behind gateway Git and OCI routes
- `cleanroom serve` foreground server plus `cleanroom daemon` lifecycle management and CLI client command set (`exec`, `console`, `sandbox inspect`, `execution inspect`)
- `cleanroom exec` RPC wrapper flow

### Phase 2
- Audit log pipeline + blocked-connection reporting
- Caching and lockfile-aware behavior improvements
- CI examples and templates

### Phase 3
- Fine-grained network controls (egress labels, protocols)
- Multi-registry and multi-language first-class support
- Remote/local policy caching and policy versioning store
- Admin override workflows + policy exceptions with expiry

## 13) Acceptance criteria (v1)
1. A repo policy can be checked in and parsed by default.
2. Running `cleanroom exec [--] <command>` creates a sandbox where unlisted hosts are unreachable.
3. Gateway-mediated Git, OCI registry, and RubyGems fetches work only through cache-backed routes and allowed destinations.
4. Unsupported destination attempts are denied and logged.
5. Git clones are rewritten to cached smart-HTTP endpoints when the target host is allowed.
6. Launch fails when selected backend cannot satisfy required policy capabilities.
7. Audit logs include execution, backend, policy, gateway action, and reason-code context where available.
8. Backend adapters must pass the Cleanroom conformance suite for required capabilities before being considered supported.
9. All CLI execution paths (`exec`, `console`, `sandbox inspect`, `execution inspect`) are routed through the control-plane API; no direct non-API execution path is supported.

Future acceptance criteria may add lockfile-derived artifact enforcement,
policy-level secret bindings, explicit deny rules, wildcard host rules, and
registry-specific fallback policy.

## 14) Conformance test matrix (required for supported backends)
Cleanroom must provide a backend-agnostic conformance suite that validates equivalent enforcement outcomes for the same `CompiledPolicy`.

Minimum matrix coverage:
- Default deny blocks unlisted destinations.
- Explicit allow host/port permits expected outbound traffic.
- Missing required capability fails launch with `backend_capability_mismatch`.

Future matrix coverage may add explicit-deny precedence, wildcard host
semantics, registry fallback behavior, lockfile enforcement, and secret scope
violations once those policy features are implemented.

Support gate:
- A backend is not marked supported in v1 until conformance tests pass on its target platform(s).
