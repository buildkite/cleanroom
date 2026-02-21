#!/usr/bin/env bash
set -euo pipefail

echo "--- :hammer: Building binaries"
mise run build

KERNEL_IMAGE="${CLEANROOM_KERNEL_IMAGE:-}"
ROOTFS_IMAGE="${CLEANROOM_ROOTFS:-}"
FIRECRACKER_BINARY="${CLEANROOM_FIRECRACKER_BINARY:-firecracker}"

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

echo "Firecracker e2e checks passed"
