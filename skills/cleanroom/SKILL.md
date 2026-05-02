---
name: cleanroom
description: Use when an agent has the Buildkite Cleanroom CLI installed and should run commands, tests, builds, shells, local services, or other work inside Cleanroom; author or validate cleanroom.yaml policy; or diagnose Cleanroom daemon, backend, sandbox, workspace-copy, network, exposure, or agent-in-sandbox behavior.
---

# Cleanroom

Use Cleanroom when the task benefits from a VM isolation boundary, deny-by-default egress, reproducible repository checkout, or a host-side control plane for untrusted work. This skill assumes `cleanroom` is already installed. Prefer direct runtime proof over code-only reasoning when the user asks whether something works.

## Start Here

1. Confirm the installed CLI is available:
   - Run `cleanroom version` when version or install state matters.
   - If the user names an exact command, validate with that exact command.
   - If flag availability is unclear, run `cleanroom <command> --help` before guessing.
2. Inspect policy before running expensive work:
   - Cleanroom reads `cleanroom.yaml`, then `.buildkite/cleanroom.yaml` as fallback.
   - Run `cleanroom policy validate` from the target repo, or `cleanroom policy validate --chdir <path>` when validating another directory.
3. Check runtime readiness when failures might be host-related:
   - `cleanroom daemon status`
   - `cleanroom doctor --json`
   - Use the selected backend from runtime config or pass `--backend firecracker` / `--backend darwin-vz` when the task requires it.

## Choose the Command

- One-shot command in the current repo:

```bash
cleanroom exec -- npm test
```

- One-shot command that needs a tty:

```bash
cleanroom exec --tty -- bash
cleanroom console -- bash
```

- Keep a sandbox for follow-up commands:

```bash
cleanroom exec --keep --print-sandbox-id -- npm test
cleanroom exec --in <sandbox-id> -- npm run lint
```

- Pre-create a repo-aware sandbox without running a workload:

```bash
SANDBOX_ID="$(cleanroom create)"
cleanroom exec --in "$SANDBOX_ID" -- npm run lint
```

- Create a repo-agnostic sandbox:

```bash
cleanroom sandbox create
```

`cleanroom exec`, `cleanroom console`, and `cleanroom create` are repo-aware when run from a git checkout. `cleanroom sandbox create` stays explicit and repo-agnostic.

## Repository And Workspace State

- By default, repo-aware commands materialize committed local `HEAD` into the guest workdir, usually `/workspace`.
- Dirty worktrees are not copied implicitly. If local edits matter, use explicit copy-in:

```bash
cleanroom exec --copy-in -- npm test
cleanroom create --copy-in
cleanroom workspace copy-in <sandbox-id>
```

- Inspect sandbox changes before copying them back:

```bash
cleanroom workspace diff <sandbox-id>
cleanroom workspace copy-out --dry-run <sandbox-id>
cleanroom workspace copy-out <sandbox-id>
```

- Copy-out is safety-oriented. Expect it to refuse when the local checkout or paths no longer match the sandbox baseline.

## Running Agents Inside Cleanroom

- Pass only the environment variables the agent needs:

```bash
cleanroom exec --tty -e OPENAI_API_KEY -- codex app-server
```

- If an agent needs writable config inside the guest, set a guest-side location deliberately:

```bash
cleanroom exec -e OPENAI_API_KEY -e CODEX_HOME=/workspace/.codex -- codex app-server
```

- Treat copied agent config and trusted guest workdirs as explicit setup requirements. Do not assume host agent configuration, MCP logins, or workspace trust automatically exist inside the VM.
- If a sandboxed agent prints a warning about config or MCP servers, inspect the guest config that was actually copied or generated instead of guessing from host state.

## Network And Policy Rules

- Repository policy should stay deny-by-default. Do not put allow-by-default behavior in `cleanroom.yaml`.
- Use exact host and port rules:

```yaml
sandbox:
  network:
    default: deny
    allow:
      - github.com:443
      - host: registry.npmjs.org
        ports: [443]
```

- For repo-aware bootstrap, allow the repository remote host, commonly `github.com:443`.
- Stage-local network blocks are separate: `repository.network`, `sandbox.dependencies.network`, `sandbox.services.network`, and `sandbox.run.network` do not inherit from each other.
- Do not mix stage-local network blocks with legacy `sandbox.network.allow`.
- Use `--dangerously-allow-all` only as a request-scoped debugging or exploration choice. Do not convert that into checked-in policy.
- Runtime/backend tuning belongs in Cleanroom runtime config, usually `~/.config/cleanroom/config.yaml`. Checked-in policy should stay backend-neutral.

## Ports And Local Services

- Raw TCP exposure:

```bash
cleanroom exec --expose 15432:5432 -- postgres
```

- HTTPS exposure under `cleanroom.localhost`:

```bash
cleanroom exec --expose-https buildkite:3000 -- npm run dev
cleanroom expose --in <sandbox-id> --expose-https buildkite:3000
cleanroom port-forward --in <sandbox-id> 15432:5432
```

- On macOS, named HTTPS exposure needs host DNS/trust setup:

```bash
sudo cleanroom dns install
cleanroom dns status
```

Exposure is request-scoped client metadata. Do not store exposure choices in `cleanroom.yaml`.

## Diagnostics

- Keep command stderr focused on guest output. When correlation matters, request IDs explicitly:

```bash
cleanroom exec --print-sandbox-id --print-trace-id -- npm test
cleanroom sandbox inspect --last
cleanroom execution inspect --last
cleanroom status --last
```

- For blocked egress, identify the active stage and exact `host:port`; then update the narrow stage allowlist or the legacy all-stage allowlist.
- For policy drift, `cleanroom policy validate` is high signal because unknown fields fail validation.
- For backend capability questions, prefer `cleanroom doctor --json` and a small smoke run on the requested host/backend.
