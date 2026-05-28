#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e-observability.sh
source "$SCRIPT_DIR/e2e-observability.sh"

DARWIN_VZ_KERNEL_IMAGE="${CLEANROOM_DARWIN_VZ_KERNEL_IMAGE:-}"
OBSERVABILITY_SUITE_LABEL="darwin-vz E2E"
OBSERVABILITY_ARCHIVE_NAME="darwin-vz-e2e-observability.tgz"

echo "--- :hammer: Building binaries"
scripts/build-go.sh

scripts/build-darwin-vz-helper.sh dist/cleanroom-darwin-vz.app

# `scripts/build-go.sh` produces host binaries in dist/, but darwin-vz doctor also
# requires a Linux guest agent binary named cleanroom-guest-agent-linux-<arch>.
host_arch="$(go env GOARCH)"
GOOS=linux GOARCH="$host_arch" CGO_ENABLED=0 go build -trimpath -o "dist/cleanroom-guest-agent-linux-$host_arch" ./cmd/cleanroom-guest-agent

helper_path="${CLEANROOM_DARWIN_VZ_HELPER:-$PWD/dist/cleanroom-darwin-vz.app}"
if [[ -d "$helper_path" ]]; then
  helper_executable="$helper_path/Contents/MacOS/cleanroom-darwin-vz"
  if [[ ! -x "$helper_executable" ]]; then
    echo "darwin-vz helper bundle is missing its executable: $helper_executable" >&2
    exit 1
  fi
elif [[ ! -x "$helper_path" ]]; then
  echo "darwin-vz helper is missing or not executable: $helper_path" >&2
  exit 1
fi

choose_local_tcp_port() {
  local port
  for _ in $(seq 1 50); do
    port=$((20000 + RANDOM % 20000))
    if ! nc -z 127.0.0.1 "$port" >/dev/null 2>&1; then
      printf '%s\n' "$port"
      return 0
    fi
  done
  echo "could not find an available local tcp port" >&2
  return 1
}

tmpdir="$(mktemp -d /tmp/cleanroom-dvz-e2e.XXXXXX)"
cleanup() {
  publish_buildkite_observability \
    "$OBSERVABILITY_SUITE_LABEL" \
    "$OBSERVABILITY_ARCHIVE_NAME" \
    "./dist/cleanroom" \
    "${listen_endpoint:-}" \
    "${tmpdir:-}" || true
  if [[ -n "${exposure_pid:-}" ]]; then
    kill "$exposure_pid" >/dev/null 2>&1 || true
    wait "$exposure_pid" >/dev/null 2>&1 || true
  fi
  if [[ -n "${suspend_sandbox_id:-}" && -n "${listen_endpoint:-}" ]]; then
    ./dist/cleanroom sandbox rm --host "$listen_endpoint" "$suspend_sandbox_id" >/dev/null 2>&1 || true
  fi
  if [[ -n "${srv_pid:-}" ]]; then
    kill "$srv_pid" >/dev/null 2>&1 || true
    wait "$srv_pid" >/dev/null 2>&1 || true
  fi
  rm -rf "$tmpdir"
}
trap cleanup EXIT

smoke_policy_dir="$tmpdir/smoke-policy"
mkdir -p "$smoke_policy_dir"
cat > "$smoke_policy_dir/cleanroom.yaml" <<'EOF'
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:91a63856cdf97b2e5659660b41d1a131d3b57bfa4cad254018e391ffef6fa4b9
  network:
    default: deny
EOF

export XDG_CONFIG_HOME="$tmpdir/c"
export XDG_CACHE_HOME="$tmpdir/cache"
export XDG_STATE_HOME="$tmpdir/s"
export XDG_RUNTIME_DIR="$tmpdir/r"
export XDG_DATA_HOME="$tmpdir/d"
export CLEANROOM_DARWIN_VZ_HELPER="$helper_path"

mkdir -p "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME" "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR" "$XDG_DATA_HOME"
mkdir -p "$XDG_CONFIG_HOME/cleanroom"
cat > "$XDG_CONFIG_HOME/cleanroom/config.yaml" <<EOF
default_backend: darwin-vz
backends:
  darwin-vz:
    network:
      mode: filehandle
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
./dist/cleanroom exec --host "$listen_endpoint" --backend darwin-vz -c "$smoke_policy_dir" -- sh -lc 'echo darwin-vz-e2e' | tee "$tmpdir/exec.out"
if ! grep -q '^darwin-vz-e2e$' "$tmpdir/exec.out"; then
  echo "expected darwin-vz smoke-test output missing" >&2
  exit 1
fi
capture_latest_execution_observability "./dist/cleanroom"
if ! require_launch_observability "$OBSERVABILITY_SUITE_LABEL"; then
  echo "server log (last 30 lines):" >&2
  tail -n 30 "$tmpdir/server.log" >&2 || true
  exit 1
fi

echo "--- :warning: Exit code propagation test"
set +e
./dist/cleanroom exec --host "$listen_endpoint" --backend darwin-vz -c "$smoke_policy_dir" -- sh -lc 'exit 9' >"$tmpdir/exit9.out" 2>"$tmpdir/exit9.err"
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

echo "--- :pause_button: Suspend/wake lifecycle smoke"
suspend_sandbox_id="$(./dist/cleanroom sandbox create --host "$listen_endpoint" --backend darwin-vz --image ghcr.io/buildkite/cleanroom-base/alpine@sha256:91a63856cdf97b2e5659660b41d1a131d3b57bfa4cad254018e391ffef6fa4b9 | tail -n 1)"
if [[ -z "$suspend_sandbox_id" ]]; then
  echo "sandbox create did not return a sandbox id" >&2
  exit 1
fi

./dist/cleanroom sandbox suspend --host "$listen_endpoint" "$suspend_sandbox_id"
./dist/cleanroom sandbox inspect --host "$listen_endpoint" "$suspend_sandbox_id" >"$tmpdir/suspend-inspect.out"
if ! grep -q '^status: suspended$' "$tmpdir/suspend-inspect.out"; then
  echo "expected sandbox to inspect as suspended" >&2
  cat "$tmpdir/suspend-inspect.out" >&2
  exit 1
fi

./dist/cleanroom exec --host "$listen_endpoint" --in "$suspend_sandbox_id" -- sh -lc 'printf after-wake >/tmp/cleanroom-suspend-wake.txt; echo command-after-wake' | tee "$tmpdir/wake-exec.out"
if ! grep -q '^command-after-wake$' "$tmpdir/wake-exec.out"; then
  echo "expected command-after-wake output missing" >&2
  exit 1
fi

./dist/cleanroom sandbox suspend --host "$listen_endpoint" "$suspend_sandbox_id"
./dist/cleanroom cp --host "$listen_endpoint" "$suspend_sandbox_id:/tmp/cleanroom-suspend-wake.txt" "$tmpdir/wake-file.txt"
if ! grep -q '^after-wake$' "$tmpdir/wake-file.txt"; then
  echo "expected copied wake file content missing" >&2
  cat "$tmpdir/wake-file.txt" >&2 || true
  exit 1
fi

./dist/cleanroom exec --host "$listen_endpoint" --in "$suspend_sandbox_id" -- sh -lc "cat > /tmp/cleanroom-http-responder <<'EOF'
#!/bin/sh
printf 'HTTP/1.1 200 OK\r\nContent-Length: 14\r\nConnection: close\r\n\r\nwake-exposure\n' | nc -l -p 18080
EOF
chmod +x /tmp/cleanroom-http-responder
/tmp/cleanroom-http-responder >/tmp/cleanroom-nc-check.log 2>&1 &
sleep 0.2
wget -q -O - http://127.0.0.1:18080/
/tmp/cleanroom-http-responder >/tmp/cleanroom-nc.log 2>&1 &" | tee "$tmpdir/exposure-guest.out"
if ! grep -q '^wake-exposure$' "$tmpdir/exposure-guest.out"; then
  echo "expected in-guest exposure response missing before suspend" >&2
  cat "$tmpdir/exposure-guest.out" >&2 || true
  exit 1
fi
./dist/cleanroom sandbox suspend --host "$listen_endpoint" "$suspend_sandbox_id"
exposure_port="$(choose_local_tcp_port)"
./dist/cleanroom port-forward --host "$listen_endpoint" --in "$suspend_sandbox_id" "$exposure_port:18080" >"$tmpdir/exposure.out" 2>"$tmpdir/exposure.err" &
exposure_pid=$!
for _ in $(seq 1 40); do
  if grep -q "tcp://127.0.0.1:$exposure_port" "$tmpdir/exposure.out"; then
    break
  fi
  if ! kill -0 "$exposure_pid" >/dev/null 2>&1; then
    echo "port-forward exited before registering exposure" >&2
    cat "$tmpdir/exposure.out" >&2 || true
    cat "$tmpdir/exposure.err" >&2 || true
    exit 1
  fi
  sleep 0.25
done
if ! grep -q "tcp://127.0.0.1:$exposure_port" "$tmpdir/exposure.out"; then
  echo "timed out waiting for port-forward registration" >&2
  cat "$tmpdir/exposure.out" >&2 || true
  cat "$tmpdir/exposure.err" >&2 || true
  exit 1
fi

set +e
curl --fail --max-time 20 --silent --show-error "http://127.0.0.1:$exposure_port/" | tee "$tmpdir/exposure-http.out"
curl_status=${PIPESTATUS[0]}
set -e
if [[ "$curl_status" -ne 0 ]]; then
  echo "local exposure request failed with status $curl_status" >&2
  echo "port-forward stdout:" >&2
  cat "$tmpdir/exposure.out" >&2 || true
  echo "port-forward stderr:" >&2
  cat "$tmpdir/exposure.err" >&2 || true
  ./dist/cleanroom exec --host "$listen_endpoint" --in "$suspend_sandbox_id" -- sh -lc 'ps; wget -S -O - http://127.0.0.1:18080/ || true' >&2 || true
  exit 1
fi
if ! grep -q '^wake-exposure$' "$tmpdir/exposure-http.out"; then
  echo "expected local exposure response missing" >&2
  cat "$tmpdir/exposure-http.out" >&2 || true
  exit 1
fi
kill "$exposure_pid" >/dev/null 2>&1 || true
wait "$exposure_pid" >/dev/null 2>&1 || true
unset exposure_pid

./dist/cleanroom sandbox rm --host "$listen_endpoint" "$suspend_sandbox_id"
suspend_sandbox_id=""

echo "darwin-vz e2e checks passed"
