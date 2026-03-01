# Codex `--yolo` on macOS (Secure Pattern)

This example shows a fail-closed workflow for running Codex in `--yolo` mode from macOS with cleanroom as the external sandbox.

As of February 28, 2026, the local `darwin-vz` backend does not enforce egress allowlists (`network.allowlist_egress=false`). Because `--yolo` disables Codex's internal approvals and sandbox, do not use local `darwin-vz` for strict-security `--yolo` runs.

Use a backend that reports `network.allowlist_egress=true` (typically `firecracker` on Linux), even when initiating the run from a macOS machine.

## Files

- `cleanroom.yaml`: digest-pinned image + deny-by-default network policy template.

## Prerequisites

- `cleanroom` CLI installed.
- `codex` CLI installed in the sandbox image referenced by `cleanroom.yaml`.
- `OPENAI_API_KEY` exported in your shell.
- A cleanroom server/backend with `network.allowlist_egress=true` (recommended: Linux + `firecracker`).
- Repository checkout available on the backend host where the command runs.

## 1) Fail closed on capability

Check local macOS backend (expected to fail strict requirement today):

```bash
cleanroom doctor --backend darwin-vz --json
```

Check strict backend host (expected to pass):

```bash
cleanroom doctor --backend firecracker --json
```

A strict `--yolo` workflow requires this capability check to be true:

```json
"network.allowlist_egress": true
```

If your backend reports `false`, stop and use a stricter backend.

## 2) Run Codex in cleanroom

Run from your repository root on the backend host (or pass `-c /path/to/repo`):

```bash
printf '%s' "$OPENAI_API_KEY" | cleanroom exec \
  --backend firecracker \
  --rm \
  -c "$PWD" \
  -- sh -lc 'codex login --with-api-key >/dev/null && codex exec --yolo "$1"' -- \
  "Run the tests, fix failures, and summarize what changed."
```

Notes:

- From macOS, run this command in an SSH session on the Linux cleanroom host.
- API key is piped over stdin to `codex login --with-api-key` (not passed as a process argument).
- `--rm` tears down the sandbox after execution.
- The policy boundary is defined by `cleanroom.yaml`.

## 3) Policy tuning

Start from this directory's `cleanroom.yaml` and keep the egress list as small as possible for your workflow.
