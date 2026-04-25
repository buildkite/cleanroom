#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Benchmark cleanroom TTI (sandbox create -> first successful command) with hyperfine.

Usage:
  scripts/benchmark-tti.sh [options]

Options:
  --host <endpoint>         Control-plane endpoint (default: unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock, or unix:///tmp/cleanroom/cleanroom.sock)
  -n, --iterations <count>  Number of benchmark runs (default: 10)
  --warmup <count>          Warmup runs before measuring (default: 1)
  --backend <name>          Optional backend override for sandbox create
  --image <ref>             Image ref for sandbox create (default: pinned cleanroom-base alpine digest)
  -c, --chdir <path>        Accepted for compatibility; raw sandbox benchmark ignores local policy
  --output-dir <path>       JSON output directory (default: benchmarks/results)
  --cleanroom-bin <path>    cleanroom binary path (default: cleanroom from PATH, then ./dist/cleanroom)
  --start-server            Start cleanroom serve in the background before benchmarking
  --build                   Run mise run build before benchmarking
  --gateway-listen <addr>   Gateway listen address when starting a server (default: :0)
  -h, --help                Show this help

Environment:
  XDG_RUNTIME_DIR           Used to derive the default unix socket endpoint.

Notes:
  - By default this script expects the cleanroom server to already be running.
  - Use --start-server to self-host cleanroom serve outside the timed section.
  - The measured command is: cleanroom sandbox create ... && cleanroom exec --in ... -- echo benchmark
  - Sandbox termination runs in hyperfine cleanup and is excluded from timing.
EOF
}

if [[ -n "${XDG_RUNTIME_DIR:-}" ]]; then
  default_host="unix://${XDG_RUNTIME_DIR}/cleanroom/cleanroom.sock"
else
  default_host="unix:///tmp/cleanroom/cleanroom.sock"
fi
default_image="ghcr.io/buildkite/cleanroom-base/alpine@sha256:fe2fbe4950546c0983247d71d5ff5795b064d7e603596efc57e2ea88aaaf3cb1"

if command -v cleanroom >/dev/null 2>&1; then
  cleanroom_bin="$(command -v cleanroom)"
elif [[ -x "./dist/cleanroom" ]]; then
  cleanroom_bin="./dist/cleanroom"
else
  cleanroom_bin="cleanroom"
fi
cleanroom_bin_explicit=0

host="$default_host"
iterations=10
warmup=1
backend=""
image="$default_image"
chdir="$PWD"
output_dir="benchmarks/results"
start_server=0
build_before=0
gateway_listen=":0"
server_pid=""
server_socket_path=""
sandbox_id_path=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host)
      host="$2"
      shift 2
      ;;
    -n|--iterations)
      iterations="$2"
      shift 2
      ;;
    --warmup)
      warmup="$2"
      shift 2
      ;;
    --backend)
      backend="$2"
      shift 2
      ;;
    --image)
      image="$2"
      shift 2
      ;;
    -c|--chdir)
      chdir="$2"
      shift 2
      ;;
    --output-dir)
      output_dir="$2"
      shift 2
      ;;
    --cleanroom-bin)
      cleanroom_bin="$2"
      cleanroom_bin_explicit=1
      shift 2
      ;;
    --start-server)
      start_server=1
      shift
      ;;
    --build)
      build_before=1
      shift
      ;;
    --gateway-listen)
      gateway_listen="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

cleanup() {
  if [[ -n "$sandbox_id_path" && -f "$sandbox_id_path" ]]; then
    sid="$(grep -m1 '^sandbox_id=' "$sandbox_id_path" | cut -d= -f2 || true)"
    if [[ -n "$sid" ]]; then
      "$cleanroom_bin" sandbox rm --host "$host" "$sid" >/dev/null 2>&1 || true
    fi
    : > "$sandbox_id_path"
  fi

  if [[ -n "$server_pid" ]]; then
    kill "$server_pid" >/dev/null 2>&1 || true
    wait "$server_pid" >/dev/null 2>&1 || true
  fi

  if [[ -n "$server_socket_path" ]]; then
    rm -f "$server_socket_path"
  fi

  if [[ -n "$sandbox_id_path" ]]; then
    rm -f "$sandbox_id_path"
  fi
}
trap cleanup EXIT

if ! [[ "$iterations" =~ ^[0-9]+$ ]] || [[ "$iterations" -le 0 ]]; then
  echo "iterations must be a positive integer" >&2
  exit 1
fi
if ! [[ "$warmup" =~ ^[0-9]+$ ]]; then
  echo "warmup must be a non-negative integer" >&2
  exit 1
fi
if ! command -v hyperfine >/dev/null 2>&1; then
  echo "hyperfine is required but not found in PATH" >&2
  exit 1
fi
if [[ "$build_before" -eq 1 ]]; then
  if ! command -v mise >/dev/null 2>&1; then
    echo "mise is required for --build but not found in PATH" >&2
    exit 1
  fi
  mise run build >/dev/null
  if [[ "$cleanroom_bin_explicit" -eq 0 && -x "./dist/cleanroom" ]]; then
    cleanroom_bin="./dist/cleanroom"
  fi
fi
if [[ "$cleanroom_bin" == */* ]]; then
  if [[ ! -x "$cleanroom_bin" ]]; then
    echo "cleanroom binary not found or not executable: $cleanroom_bin" >&2
    exit 1
  fi
elif ! command -v "$cleanroom_bin" >/dev/null 2>&1; then
  echo "cleanroom binary not found in PATH: $cleanroom_bin" >&2
  exit 1
fi
if ! command -v grep >/dev/null 2>&1; then
  echo "grep is required but not found in PATH" >&2
  exit 1
fi

mkdir -p "$output_dir"
timestamp="$(date -u +%Y-%m-%dT%H-%M-%SZ)"
output_path="${output_dir}/${timestamp}.json"
sandbox_id_path="$(mktemp "${output_dir}/.tti-sandbox-id.XXXXXX")"
server_log_path="${output_dir}/${timestamp}-server.log"

if [[ -z "${CLEANROOM_DARWIN_VZ_HELPER:-}" ]]; then
  if [[ -d "${PWD}/dist/cleanroom-darwin-vz.app" ]]; then
    export CLEANROOM_DARWIN_VZ_HELPER="${PWD}/dist/cleanroom-darwin-vz.app"
  elif [[ -x "${PWD}/dist/cleanroom-darwin-vz" ]]; then
    export CLEANROOM_DARWIN_VZ_HELPER="${PWD}/dist/cleanroom-darwin-vz"
  fi
fi

if [[ "$start_server" -eq 1 ]]; then
  if [[ "$host" == unix://* ]]; then
    server_socket_path="${host#unix://}"
    mkdir -p "$(dirname "$server_socket_path")"
    rm -f "$server_socket_path"
  fi

  "$cleanroom_bin" serve --listen "$host" --gateway-listen "$gateway_listen" >"$server_log_path" 2>&1 &
  server_pid=$!

  for _ in {1..200}; do
    if ! kill -0 "$server_pid" >/dev/null 2>&1; then
      echo "cleanroom server exited before it became ready" >&2
      tail -80 "$server_log_path" >&2 || true
      exit 1
    fi
    if "$cleanroom_bin" sandbox ls --host "$host" >/dev/null 2>&1; then
      break
    fi
    sleep 0.05
  done

  if ! "$cleanroom_bin" sandbox ls --host "$host" >/dev/null 2>&1; then
    echo "timed out waiting for cleanroom server readiness on $host" >&2
    tail -80 "$server_log_path" >&2 || true
    exit 1
  fi
fi

# Keep accepting --chdir for older callers while ensuring this benchmark stays
# repo-agnostic and never reads local cleanroom.yaml or git state.
: "$chdir"

sandbox_create_cmd=("$cleanroom_bin" sandbox create --host "$host")
if [[ -n "$backend" ]]; then
  sandbox_create_cmd+=(--backend "$backend")
fi
if [[ -n "$image" ]]; then
  sandbox_create_cmd+=(--image "$image")
fi

quoted_sandbox_create_cmd=""
for token in "${sandbox_create_cmd[@]}"; do
  printf -v escaped '%q' "$token"
  quoted_sandbox_create_cmd+="${escaped} "
done

exec_cmd_prefix=("$cleanroom_bin" exec --host "$host" --in)
quoted_exec_cmd_prefix=""
for token in "${exec_cmd_prefix[@]}"; do
  printf -v escaped '%q' "$token"
  quoted_exec_cmd_prefix+="${escaped} "
done
printf -v sandbox_id_escaped '%q' "$sandbox_id_path"
quoted_benchmark_cmd="sid=\$(${quoted_sandbox_create_cmd}2>/dev/null); printf 'sandbox_id=%s\n' \"\${sid}\" > ${sandbox_id_escaped}; ${quoted_exec_cmd_prefix}\"\${sid}\" -- echo benchmark >/dev/null"

printf -v cleanroom_bin_escaped '%q' "$cleanroom_bin"
printf -v host_escaped '%q' "$host"
cleanup_cmd="sid=\$(grep -m1 '^sandbox_id=' ${sandbox_id_escaped} | cut -d= -f2 || true); if [ -n \"\${sid}\" ]; then ${cleanroom_bin_escaped} sandbox rm --host ${host_escaped} \"\${sid}\" >/dev/null 2>&1 || true; fi; : > ${sandbox_id_escaped}"

echo "Benchmarking TTI with hyperfine"
echo "- endpoint: ${host}"
echo "- image: ${image}"
echo "- iterations: ${iterations}"
echo "- warmup: ${warmup}"
echo "- output: ${output_path}"

hyperfine \
  --runs "$iterations" \
  --warmup "$warmup" \
  --prepare "$cleanup_cmd" \
  --cleanup "$cleanup_cmd" \
  --export-json "$output_path" \
  "$quoted_benchmark_cmd"

bash -lc "$cleanup_cmd"

echo "Results written to ${output_path}"
