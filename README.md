# 🧑‍🔬 Cleanroom

Cleanroom is a secure, hermetic execution environment for modern agentic and development workflows that need repeatability, speed, supply-chain safety, and safe execution of untrusted code.

Agents and local automation often run with mixed trust levels (external prompts, unreviewed scripts, temporary tasks). Cleanroom solves this by enforcing a policy boundary: explicit, auditable egress rules around what can be reached and from where, while still allowing practical command execution.

At its core, Cleanroom gives you:
- deny-by-default networking
- explicit host/port allowlists for runtime access
- cache-mediated dependency fetches (repeatable and faster)
- safe secret injection without plaintext in repo policy or command lines
- policy-constrained sandboxes for running untrusted scripts and agent output
- pluggable backends (local execution today, remote execution tomorrow)

## Why this exists

Agentic tools and local CLIs also need package and API access to do their jobs. Cleanroom keeps dependency and network intent in one place (`cleanroom.yaml`) and makes each execution environment auditable and reproducible, so trusted launchers can execute untrusted workloads inside explicit policy boundaries.

The result is a safer baseline:
- accidental dependency drift gets reduced
- unexpected outbound traffic is blocked by default
- dependency fetches are routed through caching/proxy policy points
- local and remote execution can use the same repository policy definition

## Install / use

Once implemented:

```bash
cleanroom serve --listen unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock
cleanroom policy validate
cleanroom exec -- "npm test"
```

## Configuration: `cleanroom.yaml`

Place policy at repository root as `cleanroom.yaml` (legacy fallback: `.buildkite/cleanroom.yaml`).

```yaml
version: 1
project:
  name: example-repo
  owner: team-platform

backends:
  default: local
  allow_overrides: true

sandbox:
  ttl_minutes: 30
  network:
    default: deny
    allow:
      - host: registry.npmjs.org
        ports: [443]
      - host: api.github.com
        ports: [443]

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

metadata:
  team: platform-security
  risk_class: low
```

The policy maps to three enforcement layers:
- **network**: explicit outbound host/port controls for everything.
- **registry**: package fetches only through content-cache and allowed registries.
- **lockfile-aware dependency policy**: only allowed artifacts from lockfiles are fetched.

## Quick usage

### 1) Validate policy

```bash
cleanroom policy validate
```

### 2) Start the local control-plane server

```bash
cleanroom serve --listen unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock
```

All CLI commands (including `cleanroom exec`) talk to this API endpoint.

### 3) Run a task in a sandbox

```bash
cleanroom exec -- "npm install && npm test"
```

### 3b) Run an agentic task in a sandbox

```bash
cleanroom exec -- "agent-tool execute 'resolve docs updates and open PR branch'"
```

Use `--backend sprites` to run using the remote backend once configured:

```bash
cleanroom exec --backend sprites -- "pytest -q"
```

The same command shape works for local tools, local scripts, and agent tasks.

## Server + `exec` UX

Cleanroom uses a client/server model with one binary:

```bash
cleanroom serve --listen unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock
```

`cleanroom exec` remains the primary command UX and talks to the server API under the hood.

```bash
cleanroom exec -- "npm test"
```

Useful forms:

```bash
cleanroom exec -it -- bash
cleanroom exec -d -- "npm run watch"
cleanroom exec --sandbox dev --reuse -- "npm test"
```

Execution behavior:
- resolves policy and creates a sandbox (ephemeral by default)
- creates an execution and streams output
- returns the workload exit code
- tears down ephemeral sandboxes after execution
- first `Ctrl-C` cancels execution, second `Ctrl-C` detaches the client stream

## Mise integration (first class)

If a repository contains `.mise.toml` (or `.mise/config.toml`), `cleanroom exec` treats `mise` as part of the runtime bootstrap path.

```bash
cleanroom exec --backend local -- "mise exec npm test"
```

You can also run through implicit bootstrap:

```bash
cleanroom exec "npm test"
```

In that mode, Cleanroom:

- detects `mise` files in the workspace,
- resolves tool versions from repository-managed config,
- applies the resulting environment inside the sandbox before executing the command,
- auto-trusts copied workspace mise config paths in launched mode (`/workspace/.mise.toml`, `/workspace/.mise/config.toml`),
- and still enforces the same network/registry/secret rules.

To keep command parsing predictable, prefer the explicit form in scripts:

```bash
cleanroom exec -- "mise exec node --version"
```

You can also pin this behavior in policy:

```yaml
sandbox:
  mise:
    enabled: true
    auto_bootstrap: true
    config_files:
      - .mise.toml
      - .mise/config.toml
```

### 4) Watch / inspect

```bash
cleanroom sandboxes list
cleanroom executions get <sandbox-id> <execution-id>
cleanroom sandboxes events <sandbox-id> --follow
```

### 5) See what would run

```bash
cleanroom policy validate --json
```

This prints the resolved policy and effective network/registry plan before execution.

### 6) Diagnose host networking + inspect run timings

```bash
cleanroom doctor
cleanroom status --run-id <run-id>
```

`doctor` now reports host networking prerequisites for launched Firecracker runs (presence of `ip`/`iptables`/`sysctl`/`sudo`, plus `sudo -n` checks used by TAP/NAT setup).

`status --run-id` prints per-run observability from `run-observability.json`, including:
- policy host-resolution time
- rootfs preparation time
- Firecracker process start time
- workspace archive preparation time
- network setup time
- VM ready time (process start -> guest agent ready)
- vsock wait time (wait-to-connect for guest agent)
- guest command runtime
- network cleanup time
- total run time

## What happens at runtime

- `cleanroom exec` sends requests to the `cleanroom serve` API.
- The control plane reads policy and builds an internal execution spec.
- If both `cleanroom.yaml` and `.buildkite/cleanroom.yaml` exist, root config wins and `.buildkite` is ignored with a warning.
- The server starts the selected backend and applies the deny-by-default egress policy.
- Allowed package manager traffic is directed through `content-cache`.
- Git operations can be routed through cache as well.
- Secrets are injected only at runtime into runtime components, not from policy files.

## Safety model (developer-focused)

- No plaintext secrets in source control.
- No wildcard package/network access unless explicitly allowed.
- Lockfile-aware package enforcement to avoid unexpected dependency resolution.
- Audit logs include what was denied and why.

## Learn more

Detailed architecture and implementation plan: `SPEC.md`.
