#!/usr/bin/env bash
set -euo pipefail

echo "--- :hammer: Building binaries"
mise run build

KERNEL_IMAGE="${CLEANROOM_KERNEL_IMAGE:-}"
ROOTFS_IMAGE="${CLEANROOM_ROOTFS:-}"
FIRECRACKER_BINARY="${CLEANROOM_FIRECRACKER_BINARY:-firecracker}"
PRIVILEGED_MODE="${CLEANROOM_PRIVILEGED_MODE:-}"
PRIVILEGED_HELPER_PATH="${CLEANROOM_PRIVILEGED_HELPER_PATH:-}"

if [[ -z "$KERNEL_IMAGE" ]]; then
  echo "CLEANROOM_KERNEL_IMAGE is required for Firecracker e2e CI" >&2
  exit 1
fi
if [[ -z "$ROOTFS_IMAGE" ]]; then
  echo "CLEANROOM_ROOTFS is required for Firecracker e2e CI" >&2
  exit 1
fi

tmpdir="$(mktemp -d)"
cleanup() {
  if [[ -n "${srv_pid:-}" ]]; then
    kill "$srv_pid" >/dev/null 2>&1 || true
    wait "$srv_pid" >/dev/null 2>&1 || true
  fi
  rm -rf "$tmpdir"
}
trap cleanup EXIT

export XDG_CONFIG_HOME="$tmpdir/config"
export XDG_STATE_HOME="$tmpdir/state"
export XDG_RUNTIME_DIR="$tmpdir/runtime"
export XDG_DATA_HOME="$tmpdir/data"

mkdir -p "$XDG_CONFIG_HOME" "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR" "$XDG_DATA_HOME"
mkdir -p "$XDG_CONFIG_HOME/cleanroom"
cat > "$XDG_CONFIG_HOME/cleanroom/config.yaml" <<EOF
default_backend: firecracker
backends:
  firecracker:
    binary_path: $FIRECRACKER_BINARY
    kernel_image: $KERNEL_IMAGE
    rootfs: $ROOTFS_IMAGE
    vcpus: 2
    memory_mib: 1024
    launch_seconds: 45
EOF

if [[ -n "$PRIVILEGED_MODE" ]]; then
  echo "    privileged_mode: $PRIVILEGED_MODE" >> "$XDG_CONFIG_HOME/cleanroom/config.yaml"
fi
if [[ -n "$PRIVILEGED_HELPER_PATH" ]]; then
  echo "    privileged_helper_path: $PRIVILEGED_HELPER_PATH" >> "$XDG_CONFIG_HOME/cleanroom/config.yaml"
fi

echo "--- :stethoscope: Doctor"
./dist/cleanroom doctor --json | tee "$tmpdir/doctor.json"
if grep -q '"status": "fail"' "$tmpdir/doctor.json"; then
  echo "doctor checks reported failures" >&2
  exit 1
fi

socket_path="$tmpdir/cleanroom.sock"
listen_endpoint="unix://$socket_path"

echo "--- :rocket: Start cleanroom control-plane"
./dist/cleanroom serve --listen "$listen_endpoint" >"$tmpdir/server.log" 2>&1 &
srv_pid=$!

for _ in $(seq 1 40); do
  if [[ -S "$socket_path" ]]; then
    break
  fi
  sleep 0.25
done
if [[ ! -S "$socket_path" ]]; then
  echo "cleanroom server did not create unix socket: $socket_path" >&2
  echo "server log:" >&2
  cat "$tmpdir/server.log" >&2
  exit 1
fi

echo "--- :white_check_mark: Launched execution smoke test"
./dist/cleanroom exec --host "$listen_endpoint" -c "$PWD" -- sh -lc 'echo cleanroom-e2e' | tee "$tmpdir/exec.out"
if ! grep -q '^cleanroom-e2e$' "$tmpdir/exec.out"; then
  echo "expected smoke-test output missing" >&2
  exit 1
fi

echo "--- :warning: Exit code propagation test"
set +e
./dist/cleanroom exec --host "$listen_endpoint" -c "$PWD" -- sh -lc 'exit 7' >/dev/null 2>&1
status=$?
set -e
if [[ "$status" -ne 7 ]]; then
  echo "expected exit code 7 from guest command, got $status" >&2
  exit 1
fi

echo "--- :bar_chart: Run observability present"
./dist/cleanroom status --last-run | tee "$tmpdir/status.out"
if ! grep -q 'run-observability.json' "$tmpdir/status.out"; then
  echo "expected run-observability.json reference in status output" >&2
  exit 1
fi

obs_file="$(ls -1t "$XDG_STATE_HOME"/cleanroom/runs/*/run-observability.json 2>/dev/null | head -n 1 || true)"
if [[ -n "$obs_file" && -f "$obs_file" ]]; then
  extract_json_number() {
    local key="$1"
    local file="$2"
    sed -nE "s/.*\"${key}\"[[:space:]]*:[[:space:]]*([0-9]+).*/\1/p" "$file" | head -n 1
  }
  extract_json_string() {
    local key="$1"
    local file="$2"
    sed -nE "s/.*\"${key}\"[[:space:]]*:[[:space:]]*\"([^\"]+)\".*/\1/p" "$file" | head -n 1
  }

  run_id="$(extract_json_string run_id "$obs_file")"
  total_ms="$(extract_json_number total_ms "$obs_file")"
  policy_resolve_ms="$(extract_json_number policy_resolve_ms "$obs_file")"
  rootfs_copy_ms="$(extract_json_number rootfs_copy_ms "$obs_file")"
  network_setup_ms="$(extract_json_number network_setup_ms "$obs_file")"
  firecracker_start_ms="$(extract_json_number firecracker_start_ms "$obs_file")"
  vm_ready_ms="$(extract_json_number vm_ready_ms "$obs_file")"
  vsock_wait_ms="$(extract_json_number vsock_wait_ms "$obs_file")"
  guest_exec_ms="$(extract_json_number guest_exec_ms "$obs_file")"
  cleanup_ms="$(extract_json_number cleanup_ms "$obs_file")"

  if command -v buildkite-agent >/dev/null 2>&1; then
    annotation_file="$tmpdir/observability-annotation.md"
    cat > "$annotation_file" <<EOF
### Firecracker E2E Observability

- run id: ${run_id:-n/a}

| Metric | Value (ms) |
| --- | ---: |
| total | ${total_ms:-n/a} |
| policy resolve | ${policy_resolve_ms:-n/a} |
| rootfs copy | ${rootfs_copy_ms:-n/a} |
| network setup | ${network_setup_ms:-n/a} |
| firecracker start | ${firecracker_start_ms:-n/a} |
| vm ready | ${vm_ready_ms:-n/a} |
| vsock wait | ${vsock_wait_ms:-n/a} |
| guest exec | ${guest_exec_ms:-n/a} |
| cleanup | ${cleanup_ms:-n/a} |

Source: ${obs_file}
EOF
    buildkite-agent annotate --context cleanroom-e2e-observability --style info < "$annotation_file"
  fi
fi

echo "Firecracker e2e checks passed"
