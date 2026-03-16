#!/usr/bin/env bash
set -euo pipefail

DARWIN_VZ_KERNEL_IMAGE="${CLEANROOM_DARWIN_VZ_KERNEL_IMAGE:-}"

echo "--- :hammer: Building binaries"
scripts/build-go.sh

scripts/build-darwin-vz-helper.sh dist/cleanroom-darwin-vz

# `scripts/build-go.sh` produces host binaries in dist/, but darwin-vz doctor also
# requires a Linux guest agent binary named cleanroom-guest-agent-linux-<arch>.
host_arch="$(go env GOARCH)"
GOOS=linux GOARCH="$host_arch" CGO_ENABLED=0 go build -trimpath -o "dist/cleanroom-guest-agent-linux-$host_arch" ./cmd/cleanroom-guest-agent

helper_path="${CLEANROOM_DARWIN_VZ_HELPER:-$PWD/dist/cleanroom-darwin-vz}"
if [[ ! -x "$helper_path" ]]; then
  echo "darwin-vz helper is missing or not executable: $helper_path" >&2
  exit 1
fi

tmpdir="$(mktemp -d /tmp/cleanroom-dvz-e2e.XXXXXX)"
cleanup() {
  if [[ -n "${srv_pid:-}" ]]; then
    kill "$srv_pid" >/dev/null 2>&1 || true
    wait "$srv_pid" >/dev/null 2>&1 || true
  fi
  rm -rf "$tmpdir"
}
trap cleanup EXIT

export XDG_CONFIG_HOME="$tmpdir/c"
export XDG_STATE_HOME="$tmpdir/s"
export XDG_RUNTIME_DIR="$tmpdir/r"
export XDG_DATA_HOME="$tmpdir/d"
export CLEANROOM_DARWIN_VZ_HELPER="$helper_path"

mkdir -p "$XDG_CONFIG_HOME" "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR" "$XDG_DATA_HOME"
mkdir -p "$XDG_CONFIG_HOME/cleanroom"
cat > "$XDG_CONFIG_HOME/cleanroom/config.yaml" <<EOF
default_backend: darwin-vz
backends:
  darwin-vz:
    vcpus: 2
    memory_mib: 1024
    launch_seconds: 45
EOF
if [[ -n "$DARWIN_VZ_KERNEL_IMAGE" ]]; then
  echo "    kernel_image: $DARWIN_VZ_KERNEL_IMAGE" >> "$XDG_CONFIG_HOME/cleanroom/config.yaml"
fi

echo "--- :stethoscope: Doctor"
./dist/cleanroom doctor --backend darwin-vz --json | tee "$tmpdir/doctor.json"
if grep -q '"status": "fail"' "$tmpdir/doctor.json"; then
  echo "darwin-vz doctor checks reported failures" >&2
  exit 1
fi

socket_path="$tmpdir/cleanroom.sock"
listen_endpoint="unix://$socket_path"

echo "--- :rocket: Start cleanroom control-plane"
./dist/cleanroom serve --listen "$listen_endpoint" --gateway-listen ":0" >"$tmpdir/server.log" 2>&1 &
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
  cat "$tmpdir/server.log" >&2 || true
  exit 1
fi

echo "--- :white_check_mark: Launched execution smoke test"
./dist/cleanroom exec --host "$listen_endpoint" --backend darwin-vz -c "$PWD" -- sh -lc 'echo darwin-vz-e2e' | tee "$tmpdir/exec.out"
if ! grep -q '^darwin-vz-e2e$' "$tmpdir/exec.out"; then
  echo "expected darwin-vz smoke-test output missing" >&2
  exit 1
fi

echo "--- :warning: Exit code propagation test"
set +e
./dist/cleanroom exec --host "$listen_endpoint" --backend darwin-vz -c "$PWD" -- sh -lc 'exit 9' >"$tmpdir/exit9.out" 2>"$tmpdir/exit9.err"
status=$?
set -e
if [[ "$status" -ne 9 ]]; then
  echo "expected exit code 9 from darwin-vz guest command, got $status" >&2
  echo "stdout:" >&2
  cat "$tmpdir/exit9.out" >&2 || true
  echo "stderr:" >&2
  cat "$tmpdir/exit9.err" >&2 || true
  echo "server log (last 30 lines):" >&2
  tail -n 30 "$tmpdir/server.log" >&2 || true
  exit 1
fi

echo "--- :no_entry: Policy enforcement test"
invalid_policy_dir="$tmpdir/invalid-policy"
mkdir -p "$invalid_policy_dir"
cat > "$invalid_policy_dir/cleanroom.yaml" <<'EOF'
version: 1
sandbox:
  image:
    ref: docker.io/library/alpine@sha256:a4f4213abb84c497377b8544c81b3564f313746700372ec4fe84653e4fb03805
  network:
    default: allow
EOF

set +e
./dist/cleanroom exec --host "$listen_endpoint" --backend darwin-vz -c "$invalid_policy_dir" -- sh -lc 'echo should-not-run' >"$tmpdir/policy.out" 2>"$tmpdir/policy.err"
policy_status=$?
set -e
if [[ "$policy_status" -eq 0 ]]; then
  echo "expected invalid darwin-vz policy execution to fail" >&2
  exit 1
fi
if ! grep -q 'deny-by-default' "$tmpdir/policy.err"; then
  echo "expected deny-by-default policy error" >&2
  cat "$tmpdir/policy.err" >&2 || true
  exit 1
fi

echo "darwin-vz e2e checks passed"
