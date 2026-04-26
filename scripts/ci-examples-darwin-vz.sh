#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

DARWIN_VZ_KERNEL_IMAGE="${CLEANROOM_DARWIN_VZ_KERNEL_IMAGE:-}"

echo "--- :hammer: Building binaries"
scripts/build-go.sh

scripts/build-darwin-vz-helper.sh dist/cleanroom-darwin-vz.app

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

tmpdir="$(mktemp -d /tmp/cleanroom-dvz-examples.XXXXXX)"
cleanup() {
  if [[ -n "${srv_pid:-}" ]]; then
    kill "$srv_pid" >/dev/null 2>&1 || true
    wait "$srv_pid" >/dev/null 2>&1 || true
  fi
  rm -rf "$tmpdir"
}
trap cleanup EXIT

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

"$SCRIPT_DIR/ci-example-smoke.sh" darwin-vz "$listen_endpoint" "$PWD"

echo "darwin-vz example checks passed"
