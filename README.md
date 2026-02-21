# Cleanroom

Cleanroom is an API-first control plane for isolated command execution.

Current backend: **Firecracker** on Linux/KVM.

It is built for this lifecycle:
1. Launch a cleanroom context from repository policy.
2. Run an agent/command in an isolated Firecracker VM.
3. Terminate the cleanroom context.

## What It Does

- Compiles repository policy from `cleanroom.yaml`
- Enforces deny-by-default egress (`host:port` allow rules)
- Runs commands in a Firecracker microVM via guest agent
- Returns exit code + stdout/stderr over API
- Stores run artifacts and timing metrics for inspection

## Architecture

- Server: `cleanroom serve`
- Client: CLI and direct HTTP JSON calls
- Transport: unix socket by default (`unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock`)
- Core endpoints:
  - `POST /v1/cleanrooms/launch`
  - `POST /v1/cleanrooms/run`
  - `POST /v1/cleanrooms/terminate`
  - `POST /v1/exec` (single-call compatibility path)

## Quick Start

### 1) Validate policy

```bash
cleanroom policy validate
```

### 2) Start API server

```bash
cleanroom serve --listen unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock
```

All CLI commands (including `cleanroom exec`) talk to this API endpoint.

To expose the API over Tailscale using embedded `tsnet`:

```bash
cleanroom serve --listen tsnet://cleanroom:7777
```

Then connect with:

```bash
cleanroom exec --host tsnet://cleanroom:7777 -- "npm test"
```

To expose the API as a Tailscale Service using the local `tailscaled` daemon:

```bash
cleanroom serve --listen tssvc://cleanroom
```

This configures `svc:cleanroom` with HTTPS on port 443 and advertises the
service from this host. Connect from another tailnet device with:

```bash
cleanroom exec --host https://cleanroom.<your-tailnet>.ts.net -- "npm test"
```

### 3) Launch -> Run -> Terminate via API

```bash
cleanroom_id="$(
  curl --silent --show-error \
    --unix-socket "$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock" \
    -H 'content-type: application/json' \
    -d '{"cwd":"'"$PWD"'"}' \
    http://localhost/v1/cleanrooms/launch | jq -r '.cleanroom_id'
)"

curl --silent --show-error \
  --unix-socket "$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock" \
  -H 'content-type: application/json' \
  -d '{"cleanroom_id":"'"$cleanroom_id"'","command":["pi","run","implement the requested refactor"]}' \
  http://localhost/v1/cleanrooms/run

curl --silent --show-error \
  --unix-socket "$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock" \
  -H 'content-type: application/json' \
  -d '{"cleanroom_id":"'"$cleanroom_id"'"}' \
  http://localhost/v1/cleanrooms/terminate
```

## CLI Shortcut

`cleanroom exec` keeps a one-command flow, but lifecycle endpoints are the primary API model.

```bash
cleanroom exec -- pi run "implement the requested refactor"
```

## API Contract (Current)

### `POST /v1/cleanrooms/launch`

Request:

```json
{
  "cwd": "/path/to/repo",
  "backend": "firecracker",
  "options": {
    "run_dir": "/tmp/cleanroom-runs",
    "read_only_workspace": false,
    "launch_seconds": 30
  }
}
```

Response fields:
- `cleanroom_id`
- `backend`
- `policy_source`
- `policy_hash`
- `run_dir_root`

### `POST /v1/cleanrooms/run`

Request:

```json
{
  "cleanroom_id": "cr-123",
  "command": ["pi", "run", "implement the requested refactor"]
}
```

Response fields:
- `run_id`
- `exit_code`
- `stdout`
- `stderr`
- `run_dir`
- `plan_path`

### `POST /v1/cleanrooms/terminate`

Request:

```json
{
  "cleanroom_id": "cr-123"
}
```

Response fields:
- `terminated`

## Policy File

Repository policy path resolution:
1. `cleanroom.yaml`
2. `.buildkite/cleanroom.yaml` (fallback)

Minimal example:

```yaml
version: 1
sandbox:
  network:
    default: deny
    allow:
      - host: api.github.com
        ports: [443]
      - host: registry.npmjs.org
        ports: [443]
```

## Firecracker Runtime Config

Runtime config path: `$XDG_CONFIG_HOME/cleanroom/config.yaml` (or `~/.config/cleanroom/config.yaml`).

Example:

```yaml
default_backend: firecracker
workspace:
  access: rw
backends:
  firecracker:
    binary_path: firecracker
    kernel_image: /opt/cleanroom/vmlinux
    rootfs: /opt/cleanroom/rootfs.ext4
    vcpus: 2
    memory_mib: 1024
    guest_cid: 3
    launch_seconds: 30
```

## Isolation Model

- Workload runs in a Firecracker microVM
- Workspace is copied per run and sent to the guest agent
- Workspace can be read-only (`workspace.access: ro` or request override)
- Host egress is controlled with TAP + iptables rules from compiled policy
- Default network behavior is deny
- Rootfs writes are discarded after each run

## Performance Notes

- Rootfs copy uses clone/reflink when available, with copy fallback
- Per-run observability is written to `run-observability.json`
- Metrics include:
  - rootfs preparation time
  - network setup time
  - VM ready time
  - guest command runtime
  - total run time

Inspect:

```bash
cleanroom doctor
cleanroom status --run-id <run-id>
```

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

## Host Requirements

- Linux host
- `/dev/kvm` available and writable
- Firecracker binary installed
- Kernel image + rootfs image configured
- `sudo -n` access for `ip`, `iptables`, and `sysctl` (network setup/cleanup)

## References

- API design notes: `API_CONNECTRPC.md`
- Full spec and roadmap: `SPEC.md`
