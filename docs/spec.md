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
Cleanroom specifies the normative security contract for a run:
- Policy semantics and match behavior (`allow`/`deny`, precedence, defaults).
- Runtime invariants (policy load timing, immutability during run lifetime).
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
- As a developer, I can run repository-defined toolchains (for example via `mise`) inside the sandbox with the same command patterns as local tooling.

## 5) Policy model
### 5.1 Repository config
Repository policy file resolution (in order):
1. `cleanroom.yaml` in repository root
2. `.buildkite/cleanroom.yaml` (fallback path)

If both exist, root `cleanroom.yaml` is authoritative and `.buildkite/cleanroom.yaml` is ignored with a warning.

```yaml
version: 1
project:
  name: my-repo

sandbox:
  ttl_minutes: 60
  mise:
    enabled: true
    auto_bootstrap: true
    config_files:
      - .mise.toml
      - .mise/config.toml
  network:
    default: deny
    allow:
      - host: api.github.com
        reason: source-control and release checks
        ports:
          - 443
      - host: *.npmjs.org
        reason: npm metadata + tgz
        ports: [443]
      - host: registry.npmjs.org
        reason: npm package tarballs
        ports: [443]
    deny:
      - host: 169.254.169.254
      - host: metadata.google.internal

registries:
  npm:
    enable: true
    allowed_hosts:
      - registry.npmjs.org
      - registry.yarnpkg.com
    cache_ref: content-cache
    fallback: deny
    lockfile_enforcement:
      enabled: true
      mode: deny_unknown
      lockfiles:
        - package-lock.json
        - yarn.lock
  pip:
    enable: false
    lockfile_enforcement:
      enabled: false

metadata:
  owner: team-security
  risk_class: low
```

### 5.2 Schema rules
- Required: `version`, `sandbox.network.default`.
- `sandbox.network.default` must be either `deny` or `allow`; v1 default must be `deny`.
- Host matching supports:
  - exact host (`registry.npmjs.org`)
  - wildcard subdomains (`*.example.com`)
  - optional CIDR if we need API/IP exceptions later.
- All hostnames are normalized to lowercase punycode when applicable.
- Ports are optional; if omitted, default allow for `{80,443}` only.
- `registries.*.fallback = deny|allow` defines handling for a supported package manager request not explicitly in allowlist.
- `registries.*.lockfile_enforcement` controls whether package fetches are constrained by lockfile-derived coordinates:
  - `enabled: true|false`
  - `mode`:
    - `deny_unknown` (default): deny any artifact not declared in resolved lockfiles.
    - `warn`: emit violation events but allow in migration mode.
    - `off`: no lockfile restrictions.
  - `lockfiles[]`: repository-relative lockfile paths.
- When lockfile enforcement is enabled, the policy compiler records exact package artifact allow entries with integrity metadata where available.

### 5.2.1 Deterministic network match semantics (normative)
- Cleanroom must normalize policy hosts and requested destination hosts before matching:
  - lowercase
  - IDN to punycode where applicable
  - trailing dot removed
- Rule precedence for destination evaluation must be deterministic:
  1. explicit `deny` exact host/IP match
  2. explicit `allow` exact host/IP match
  3. explicit `allow` wildcard host match
  4. fallback to `sandbox.network.default`
- Wildcard host matching supports only left-most label form (for example `*.example.com`).
- Wildcard host entries do not match the apex domain (`example.com`) unless explicitly listed.
- If a destination is expressed as an IP literal, it is evaluated against explicit IP/CIDR rules only.
- If no explicit IP/CIDR rule matches an IP-literal destination, default behavior is deny.

### 5.3 Secret references
- Policy contains only secret identifiers, never plaintext token values.
- Secret IDs are resolved at run-time from the CI environment or external secret provider.
- Runtime policy object contains:
  - `secret_id` (e.g. `npm_readonly`, `github_pat_ci`)
  - `target` (which backend uses it: content-cache, secret-proxy, or direct env-injected)
  - `allowed_hosts` (host restrictions for each secret binding)
  - `ttl_seconds` and optional `single_use` hints.
- Secret material is provisioned only to the cleanroom control process and never mounted into the guest filesystem.

### 5.4 Execution model
- `cleanroom exec` is the primary command for running arbitrary commands in sandbox.
- `cleanroom exec` uses a `--` command separator consistent with common shell tooling.
- Unless an explicit vector command form is added later, `cleanroom exec` defaults to shell execution (e.g., `/bin/sh -lc`) so commands like `cleanroom exec "npm test"` work directly.
- Cleanroom uses a client/server architecture:
  - `cleanroom` CLI resolves and compiles policy from repository files.
  - `cleanroom serve` validates compiled policy and executes runs via backend adapters.
  - all CLI commands, including `cleanroom exec`, call the server API.
  - "local execution" means local backend selected by the server, not a direct non-API code path.
- Current API/runtime intent: no host workspace mount input is accepted by `CreateSandbox` or `CreateExecution`.
- Workloads run against the backend-provided sandbox image filesystem for each execution.

### 5.4.1 `cleanroom exec` behavior contract (normative)
- `cleanroom exec` must:
  1. resolve API endpoint (`--host`, env, context, default unix socket),
  2. resolve and compile policy,
  3. create or select sandbox,
  4. create execution,
  5. stream output/events to caller,
  6. return workload exit code.
- Default mode is ephemeral sandbox per invocation unless explicit reuse is requested.
- Interactive mode (`-it`) must use bidirectional stream semantics.
- Non-interactive mode must use server-streaming semantics.
- First interrupt signal should request execution cancel; second interrupt may detach client stream immediately.

### 5.5 Compiled policy payload (normative)
Cleanroom compiles repository policy into an immutable `CompiledPolicy` payload for run creation. This payload is the only policy input to backend adapters.

Minimum required fields:
- `policy_hash` (digest of full compiled payload)
- `source`
  - `repository`
  - `commit_sha`
  - `policy_path`
- `network`
  - `default_action`
  - `allow_rules[]` (normalized host/IP, ports, protocol defaults)
  - `deny_rules[]` (normalized host/IP, ports optional)
- `registries`
  - manager key (`npm`, `pip`, and so on)
  - `enabled`
  - `allowed_hosts[]`
  - `cache_ref`
  - `fallback`
  - `lockfile_policy` (enabled/mode/inputs)
  - `artifact_allowlist[]` (when lockfile enforcement is enabled)
- `secrets`
  - secret binding metadata only (ID, target component, host scope, TTL hints)

Requirements:
- Backend adapters must not re-resolve policy from repository files.
- Runtime behavior is derived only from `CompiledPolicy`.

## 6) Runtime behavior
### 6.1 Launch flow
All runtime launch behavior is initiated by control-plane API calls (for example `CreateSandbox` and `CreateExecution`) from CLI or SDK clients.

1. CLI/SDK client resolves spec file using precedence above.
2. CLI/SDK client compiles policy and sends it in `CreateSandbox`.
3. Control plane validates compiled policy and backend selection.
4. Start sandbox via selected backend.
5. Attach/enable content-cache proxy sidecar or endpoint wiring.
6. Enforce runtime policy:
  - DNS/egress allowlist only.
  - outbound packet filtering to allowed host:port/protocol.
7. Run workload command.
8. Emit structured events + exit status; tear down resources.

### 6.1.1 Policy load and immutability
- Policy is loaded and compiled by the client, then provided to the control plane at sandbox creation.
- Backend adapters receive a compiled immutable policy payload; they must not receive repository file paths for policy re-resolution.
- Active runs do not support runtime policy mutation.
- Backend adapters must not re-read repository policy files after run creation.
- Guest workloads cannot mutate control-plane policy inputs for the active run.

### 6.2 Package registry filtering with content-cache
- All package manager egress flows are redirected through content-cache endpoint.
- content-cache applies registry allowlist before forwarding.
- Cache serves:
  - positive cache (hit/miss)
  - optional metadata signing/validation hooks (future extension).
- Unsupported registry requests are denied with explicit audit reason.

### 6.3 Default-fail semantics
- Any destination not matched by explicit allowlist is denied.
- Any registry not listed in an enabled `registries.*` block is denied.
- Failed policy validation blocks launch unless explicitly bypassed by an explicit admin flag.

### 6.4 Safe git clone path (content-cache)
- Cleanroom rewrites clone URLs to `content-cache` when a repo host is in `sandbox.network.allow`.
- Build flow:
  - `content-cache` is started with `--git-allowed-hosts` set to the resolved allowlist hosts.
  - cleanroom writes scoped Git URL rewrite config (for example `http://127.0.0.1:8080/git/`).
  - Clone commands run unchanged inside sandbox (`https://github.com/org/repo.git`), with transport rewritten to cache endpoint.
- Upstream auth is never carried in the policy file:
  - credentials are declared in a `content-cache` credentials template.
  - template values are resolved from environment files or secret providers at proxy startup.
- Enforcement:
  - deny by default except allowlisted Git hosts.
  - optional upstream credential rules via repo prefix matching.
  - `content-cache` can run as transparent cache + offline fallback for warm entries.

### 6.5 Secret injection plane (tokenizer-style)
- For outbound HTTPS calls that need per-service credentials, use a dedicated proxy service with:
  - secret ciphertext in request headers (sealed outside sandbox),
  - target-host allowlist per secret,
  - header/destination rewrite in the proxy.
- Behavior:
  - Clients send HTTP(S) requests through proxy with sealed secret metadata.
  - Proxy validates host scope and authorization metadata before decrypting.
  - Proxy injects token into request header (default `Authorization: Bearer` pattern), then forwards upstream.
- Cleanroom requirement:
  - no plaintext secrets passed in repository policy.
  - no plaintext secrets passed in command lines.
  - blocked if host scope on a secret does not match request host.
- Implementation note:
  - Start with an internal Cleanroom proxy shaped like `superfly/tokenizer`, then remove in favor of hardened first-party service if/when built.

### 6.6 Lockfile-restricted package fetches
- During policy compilation, cleanroom parses lockfiles from `registries.*.lockfile_enforcement.lockfiles`.
- For each allowed package manager, cleanroom builds an explicit artifact allowlist:
  - package identity + version
  - registry endpoint
  - optional integrity/hash requirement
- content-cache receives these allow rules and can only forward requests that match them.
- If a request misses lockfile constraints:
  - `mode=deny_unknown`: block and emit `lockfile_violation` event.
  - `mode=warn`: allow but emit warning/metric.
- Unsupported or missing lockfile + enabled enforcement blocks launch by default.
- Lockfile-derived restrictions are an additional layer on top of network/host allowlists, never a replacement.

## 7) Backend abstraction
Cleanroom provides a Go adapter interface for backend implementations:

- `Adapter`
  - `Name() string`
  - `Run(ctx, RunRequest) (*RunResult, error)`
- `PersistentSandboxAdapter` (extends `Adapter`)
  - `ProvisionSandbox(ctx, ProvisionRequest) error`
  - `RunInSandbox(ctx, RunRequest, OutputStream) (*RunResult, error)`
  - `TerminateSandbox(ctx, sandboxID) error`
- `StreamingAdapter` (extends `Adapter`)
  - `RunStream(ctx, RunRequest, OutputStream) (*RunResult, error)`

### 7.1 Backend capability contract (required for launch)
Each backend must publish a capability map consumed by launch-time validation. Capabilities describe enforcement outcomes, not implementation details.

Required capabilities:
- `network_default_deny`: backend can enforce deny-by-default outbound network behavior.
- `network_host_port_filtering`: backend can enforce host/port allow/deny outcomes from compiled policy.
- `dns_control_or_equivalent`: backend can prevent bypass of policy via unmanaged resolver paths.
- `policy_immutability`: backend runs against immutable compiled policy payload for the run lifetime.
- `audit_event_emission`: backend can emit required allow/deny and violation events with run identifiers.
- `secret_isolation`: backend can satisfy secret exposure constraints defined in this spec.

Optional capabilities (may be added by policy requirements later):
- `protocol_granularity`: protocol-specific network controls beyond baseline host/port behavior.
- `advanced_destination_identity`: stronger destination identity checks beyond hostname matching.
- `offline_cache_mode`: enforce no-upstream behavior when only warm cache artifacts are allowed.

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
- Firecracker microVM with per-run TAP networking and nftables enforcement.
- Primary use: developer workflows and lightweight local CI.
- Controls at runtime via host nftables rules + managed DNS from compiled policy.

## 8) Configuration and integration points
- CLI command set (v1):
  - `cleanroom serve`
  - `cleanroom policy validate`
  - `cleanroom exec [--] <command>`
  - `cleanroom console [--] <command>`
  - `cleanroom doctor`
  - `cleanroom status`
  - `cleanroom image pull|ls|rm|import|bump-ref`
- CI integration:
  - `cleanroom exec --` wrapper for existing and local automation
  - machine-readable output (`--json`) for pipeline tooling
- API/SDK (v1):
  - ConnectRPC `SandboxService` (`CreateSandbox`, `GetSandbox`, `ListSandboxes`, `DownloadSandboxFile`, `TerminateSandbox`, `StreamSandboxEvents`)
  - ConnectRPC `ExecutionService` (`CreateExecution`, `GetExecution`, `CancelExecution`, `StreamExecution`, `AttachExecution`)

### 8.1 CLI and API failure contract (normative)
CLI:
- Validation failures (`cleanroom policy validate`, pre-launch compile errors) return non-zero and print structured error details.
- Launch failures (including `backend_capability_mismatch`) return non-zero before workload execution starts.
- Runtime policy denies do not change process semantics unless deny prevents command completion; deny events must still be emitted.

API:
- `SandboxService.CreateSandbox` returns client error for invalid policy input and conflict/error response for unsatisfied backend requirements.
- `SandboxService.GetSandbox` and `ExecutionService.GetExecution` must expose terminal status, exit code, and normalized failure reason (if any).
- ConnectRPC errors must include stable application `code` and human-readable `message`.
- `ExecutionService.StreamExecution` and `ExecutionService.AttachExecution` must terminate cleanly with final exit status signaling.
- If an HTTP/JSON gateway is exposed, it must preserve the same stable error codes and reason semantics.

## 9) Audit and observability
- Emit structured logs per sandbox with:
  - `run_id`
  - `actor`
  - `backend`
  - `timestamp`
  - policy digest (`policy_hash`) of the compiled effective policy payload
  - policy version hash
  - effective allowlist summary
  - backend, command, user/actor
  - blocked connection attempts (host, reason, timestamp)
- Deny events must use stable reason codes (for example `host_not_allowed`, `registry_not_allowed`, `lockfile_violation`, `backend_capability_mismatch`).
- lockfile violations (registry, package, version, requested_path, action)
- Metrics:
  - launch success/fail by backend
  - rule violation counts
  - cache hit/miss latency and error rates
- Longitudinal audit retention: at least 90 days minimum.

### 9.1 Stable reason/error code set (v1 baseline)
Cleanroom must standardize and document a canonical code enum used consistently across CLI output, API responses, and audit events.

Minimum v1 codes:
- `policy_invalid`
- `policy_conflict`
- `backend_unavailable`
- `backend_capability_mismatch`
- `host_not_allowed`
- `registry_not_allowed`
- `lockfile_violation`
- `secret_scope_violation`
- `runtime_launch_failed`

## 10) Security considerations
- Principle of least privilege:
  - deny-by-default network, explicit allow rules only.
- Tamper-evident policy changes:
  - policy file changes require review before merge.
- Secret safety:
  - no embedding secrets in policy file.
  - secrets injected at runtime by CI secret store.
- Supply chain safety:
  - content-cache as first stop for registry access.
  - support offline modes where only pre-warmed cached artifacts are permitted.
- Secret safety:
  - no plaintext secrets in policy, command args, or guest env.
  - secret IDs are validated against policy and projected as short-lived bindings only.
  - all injection events are logged with secret ID, destination, and reason, never with secret values.

## 11) Risks and open decisions
- Whether tokenizer-style injection runs as embedded process in cleanroom binary or as isolated helper.
- Hostname matching behavior under SNI/TLS certificates and proxy chains.
- Whether to allow dynamic hostlist generation (e.g., from lockfiles).
- Policy precedence model for parent/child directories and monorepo overrides.
- Whether all targeted ecosystems have reliable lockfile parser coverage and how to handle malformed locks.

## 12) Build plan
### Phase 1 (MVP)
- Spec schema + validator (`cleanroom.yaml` parser with `.buildkite/cleanroom.yaml` fallback)
- Core policy compiler to normalized allowlist + registry map
- Local backend (Firecracker) implementation
- content-cache wrapper integration for npm and one additional manager
- `cleanroom serve` daemon plus CLI client command set (`exec`, `sandboxes`, `executions`)
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
3. Package fetches work only through content-cache and allowed registries.
4. Unsupported destination attempts are denied and logged.
5. Lockfile-enabled registries block undeclared package artifacts by default.
6. Git clones are rewritten to cached smart-HTTP endpoints and private clone auth is provided without plaintext exposure.
7. Secrets can be used via tokenizer-style proxy flow with host-scoped allowlist and no plaintext secrets in guest/runtime-visible config.
8. Launch fails when selected backend cannot satisfy required policy capabilities.
9. Audit logs include `run_id`, `actor`, `backend`, and `policy_hash` for every run.
10. Backend adapters must pass the Cleanroom conformance suite for required capabilities before being considered supported.
11. All CLI execution paths (`exec`, `sandboxes`, `executions`) are routed through the control-plane API; no direct non-API execution path is supported.

## 14) Conformance test matrix (required for supported backends)
Cleanroom must provide a backend-agnostic conformance suite that validates equivalent enforcement outcomes for the same `CompiledPolicy`.

Minimum matrix coverage:
- Default deny blocks unlisted destinations.
- Explicit allow host/port permits expected outbound traffic.
- Explicit deny overrides matching allow.
- Wildcard host semantics match spec normalization/precedence.
- Registry fallback and allowlist behavior matches policy.
- Lockfile enforcement blocks undeclared artifacts in `deny_unknown` mode.
- Secret scope violations emit `secret_scope_violation`.
- Missing required capability fails launch with `backend_capability_mismatch`.

Support gate:
- A backend is not marked supported in v1 until conformance tests pass on its target platform(s).
