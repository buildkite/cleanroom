# Cleanroom Specification

## 1) Vision
Cleanroom provides repository-scoped sandbox profiles that define exactly what outbound network access and package registries a build/test run may use. A sandbox can be instantiated from these rules, enforcing deny-by-default egress while still allowing required external dependencies. Package registry traffic is routed through and filtered by a content cache layer (`https://github.com/wolfeidau/content-cache`) to improve hermeticity, repeatability, and policy enforcement.

Initial execution backends:
- **Remote sandbox:** `sprites.dev`
- **Local sandbox:** `https://github.com/jingkaihe/matchlock`

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
- Two sandbox backends with a common adapter interface.
- Host-level network allowlisting + package-registry mediation via content-cache.
- Local command-line + API surface for creating and managing runs.
- Logs, metrics, and audit metadata capture.

### Out of scope (v1)
- Full kernel-level deep packet inspection / DLP.
- Per-command inline prompt-level policy decisions.
- Multi-cloud remote scheduler federation beyond configured remote backend.

## 4) User stories
- As a maintainer, I can check in a policy file and ensure every build run can access only approved hosts.
- As a developer, I can run a command that executes my existing test commands inside a compliant sandbox.
- As a security reviewer, I can see exactly which hosts and registries were allowed for each sandbox execution.
- As an SRE, I can switch a repo between local and remote backends from policy or CLI options.
- As a developer, I can run repository-defined toolchains (for example via `mise`) inside the sandbox with the same command patterns as local tooling.

## 5) Policy model
### 5.1 Repository config
Repository policy file resolution (in order):
1. `cleanroom.yaml` in repository root
2. `.buildkite/cleanroom.yaml` (legacy/fallback path)

If both exist, root `cleanroom.yaml` is authoritative and `.buildkite/cleanroom.yaml` is ignored with a warning.

```yaml
version: 1
project:
  name: my-repo

backends:
  default: local
  allow_overrides: true

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
- Required: `version`, `backends.default`, `sandbox.network.default`.
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

### 5.3 Secret references
- Policy contains only secret identifiers, never plaintext token values.
- Secret IDs are resolved at run-time from the CI environment or external secret provider.
- Runtime policy object contains:
  - `secret_id` (e.g. `npm_readonly`, `github_pat_ci`)
  - `target` (which backend uses it: content-cache, tokenizer-broker, or direct env-injected)
  - `allowed_hosts` (host restrictions for each secret binding)
  - `ttl_seconds` and optional `single_use` hints.
- Secret material is provisioned only to the cleanroom control process and never mounted into the guest filesystem.

### 5.4 Execution model
- `cleanroom exec` is the primary command for running arbitrary commands in sandbox.
- `cleanroom exec` uses a `--` command separator consistent with common shell tooling.
- Unless an explicit vector command form is added later, `cleanroom exec` defaults to shell execution (e.g., `/bin/sh -lc`) so commands like `cleanroom exec "npm test"` work directly.
- `cleanroom run --cmd` exists for compatibility while migrating consumers.

## 6) Runtime behavior
### 6.1 Launch flow
1. Resolve spec file using precedence above.
2. Read repo policy and merge with organization defaults (if provided).
2. Validate schema and fail fast on invalid/overlapping host conflicts.
3. Compile network policy to backend-specific config.
4. Start sandbox via selected backend.
5. Attach/enable content-cache proxy sidecar or endpoint wiring.
6. Enforce runtime policy:
  - DNS/egress allowlist only.
  - outbound packet filtering to allowed host:port/protocol.
7. If `sandbox.mise.enabled` is true and a `.mise.toml` or `.mise/config.toml` exists:
   - resolve toolchain versions from repo config,
   - run workload through `mise exec` or equivalent shim behavior,
   - preserve existing policy and secret bindings.
8. Run workload command.
9. Emit structured events + exit status; tear down resources.

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
Introduce provider interface:

- `BackendAdapter`
  - `name()`
  - `provision(spec)`
  - `run(command, env, volumes, timeout, artifacts)`
  - `shutdown(id)`
  - `health()`

#### Local backend (matchlock)
- Use local daemon/runtime to create network namespace and process boundary.
- Primary use: developer workflows and lightweight local CI.
- Controls at runtime via local firewall + DNS deny/allow rules.

#### Remote backend (sprites)
- Remote execution API receives a signed execution spec.
- Local orchestration layer only submits task and streams logs/status.
- Enforce policy locally via generated network policy payload passed to provider.
- Must confirm provider-native ability for host-level deny-by-default; if unavailable, enforce via local egress tunnel + reverse proxy model.

## 8) Configuration and integration points
- CLI command set (v1):
  - `cleanroom policy validate`
  - `cleanroom policy apply`
  - `cleanroom exec [--] <command>`
  - `cleanroom run --cmd "npm test" --backend <local|sprites>` (compat alias)
  - `cleanroom status --id <run-id>`
- CI integration:
  - `cleanroom exec --` wrapper for existing and local automation
  - machine-readable output (`--json`) for pipeline tooling
- Optional API/SDK:
  - POST `/sandboxes` to create
  - GET `/sandboxes/{id}` for status/events

## 9) Audit and observability
- Emit structured logs per sandbox with:
  - policy version hash
  - effective allowlist summary
  - backend, command, user/actor
  - blocked connection attempts (host, reason, timestamp)
- lockfile violations (registry, package, version, requested_path, action)
- Metrics:
  - launch success/fail by backend
  - rule violation counts
  - cache hit/miss latency and error rates
- Longitudinal audit retention: at least 90 days minimum.

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
- Exact semantics of `sprites.dev` execution interface and policy transport.
- Whether matchlock can enforce DNS + port restrictions as tightly as required.
- Whether tokenizer-style injection runs as embedded process in cleanroom binary or as isolated helper.
- Hostname matching behavior under SNI/TLS certificates and proxy chains.
- Whether to allow dynamic hostlist generation (e.g., from lockfiles).
- Policy precedence model for parent/child directories and monorepo overrides.
- Whether all targeted ecosystems have reliable lockfile parser coverage and how to handle malformed locks.

## 12) Build plan
### Phase 1 (MVP)
- Spec schema + validator (`cleanroom.yaml` parser with `.buildkite/cleanroom.yaml` fallback)
- Core policy compiler to normalized allowlist + registry map
- Local backend (matchlock) proof-of-concept
- content-cache wrapper integration for npm and one additional manager
- CLI `validate` and `exec` (with `run --cmd` compatibility path)

### Phase 2
- Sprites backend adapter with parity behavior
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
