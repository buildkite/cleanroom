#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Benchmark repo-aware repository bootstrap under isolated cold/warm host-cache scenarios.

Usage:
  scripts/benchmark-repository-bootstrap.sh [options]

Options:
  --scenario <name>         Benchmark scenario: cold-host | warm-repository-store | warm-workspace-stage
                            (default: cold-host)
  -n, --iterations <count>  Number of measured runs (default: 5)
  --warmup <count>          Number of warmup runs before measuring (default: 1)
  --backend <name>          Optional backend override for cleanroom create
  -c, --chdir <path>        Repository/policy directory to benchmark (default: current directory)
  --output-dir <path>       Output directory (default: benchmarks/results)
  --cleanroom-bin <path>    cleanroom binary path (default: cleanroom from PATH, then ./dist/cleanroom)
  --timeout <seconds>       Server readiness timeout per run (default: 20)
  --keep-temp-dir           Keep the temporary benchmark directory instead of removing it
  -h, --help                Show this help

Notes:
  - This script starts its own cleanroom server for each seed/run sequence.
  - It isolates XDG cache/state/data/runtime directories per scenario while
    preserving the caller's runtime config discovery.
  - The measured command is: cleanroom create --json
  - The created sandbox is terminated after each run.
EOF
}

if command -v cleanroom >/dev/null 2>&1; then
  cleanroom_bin="$(command -v cleanroom)"
elif [[ -x "./dist/cleanroom" ]]; then
  cleanroom_bin="./dist/cleanroom"
else
  cleanroom_bin="cleanroom"
fi

scenario="cold-host"
iterations=5
warmup=1
backend=""
chdir="$PWD"
output_dir="benchmarks/results"
timeout_seconds=20
keep_temp_dir=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --scenario)
      scenario="$2"
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
      shift 2
      ;;
    --timeout)
      timeout_seconds="$2"
      shift 2
      ;;
    --keep-temp-dir)
      keep_temp_dir=1
      shift
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

case "$scenario" in
  cold-host|warm-repository-store|warm-workspace-stage)
    ;;
  *)
    echo "scenario must be one of: cold-host, warm-repository-store, warm-workspace-stage" >&2
    exit 1
    ;;
esac

if ! [[ "$iterations" =~ ^[0-9]+$ ]] || [[ "$iterations" -le 0 ]]; then
  echo "iterations must be a positive integer" >&2
  exit 1
fi
if ! [[ "$warmup" =~ ^[0-9]+$ ]]; then
  echo "warmup must be a non-negative integer" >&2
  exit 1
fi
if ! [[ "$timeout_seconds" =~ ^[0-9]+$ ]] || [[ "$timeout_seconds" -le 0 ]]; then
  echo "timeout must be a positive integer" >&2
  exit 1
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
if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required but not found in PATH" >&2
  exit 1
fi

chdir="$(cd "$chdir" && pwd)"
mkdir -p "$output_dir"

timestamp="$(date -u +%Y-%m-%dT%H-%M-%SZ)"
output_path="${output_dir}/${timestamp}-repository-bootstrap-${scenario}.json"
tmp_root="$(mktemp -d "/tmp/crb.XXXXXX")"
socket_root="$(mktemp -d "/tmp/crb-sock.XXXXXX")"

cleanup() {
  if [[ "$keep_temp_dir" -eq 0 ]]; then
    rm -rf "$tmp_root"
    rm -rf "$socket_root"
  fi
}
trap cleanup EXIT

now_ns() {
  if command -v python3 >/dev/null 2>&1; then
    python3 - <<'PY'
import time
print(time.time_ns())
PY
    return
  fi
  if command -v perl >/dev/null 2>&1; then
    perl -MTime::HiRes=time -e 'printf "%.0f\n", time() * 1000000000'
    return
  fi
  if date +%s%N >/dev/null 2>&1; then
    date +%s%N
    return
  fi
  printf '%s000000000\n' "$(date +%s)"
}

run_cleanroom() {
  local env_root="$1"
  shift
  mkdir -p \
    "$env_root/cache" \
    "$env_root/state" \
    "$env_root/data" \
    "$env_root/runtime"
  XDG_CACHE_HOME="$env_root/cache" \
  XDG_STATE_HOME="$env_root/state" \
  XDG_DATA_HOME="$env_root/data" \
  XDG_RUNTIME_DIR="$env_root/runtime" \
    "$cleanroom_bin" "$@"
}

cleanroom_state_dir() {
  local env_root="$1"
  printf '%s\n' "$env_root/state/cleanroom"
}

cleanroom_cache_dir() {
  local env_root="$1"
  printf '%s\n' "$env_root/cache/cleanroom"
}

reset_stage_state() {
  local env_root="$1"
  rm -rf \
    "$(cleanroom_state_dir "$env_root")/snapshots" \
    "$(cleanroom_state_dir "$env_root")/executions" \
    "$(cleanroom_cache_dir "$env_root")/stage-caches"
}

reset_transport_state() {
  local env_root="$1"
  rm -rf \
    "$(cleanroom_state_dir "$env_root")/repos" \
    "$(cleanroom_cache_dir "$env_root")/content-cache"
}

stop_server() {
  local pid="$1"
  if [[ -n "$pid" ]] && kill -0 "$pid" >/dev/null 2>&1; then
    kill "$pid" >/dev/null 2>&1 || true
    wait "$pid" >/dev/null 2>&1 || true
  fi
}

wait_for_server() {
  local env_root="$1"
  local host="$2"
  local pid="$3"
  local log_path="$4"
  local deadline_ns
  deadline_ns=$(( $(now_ns) + timeout_seconds * 1000000000 ))

  while true; do
    if ! kill -0 "$pid" >/dev/null 2>&1; then
      echo "cleanroom serve exited before becoming ready" >&2
      echo "server log: $log_path" >&2
      if [[ -f "$log_path" ]]; then
        cat "$log_path" >&2
      fi
      return 1
    fi
    if run_cleanroom "$env_root" sandbox ls --host "$host" >/dev/null 2>&1; then
      return 0
    fi
    if (( $(now_ns) > deadline_ns )); then
      echo "timed out waiting for cleanroom serve readiness at $host" >&2
      echo "server log: $log_path" >&2
      if [[ -f "$log_path" ]]; then
        cat "$log_path" >&2
      fi
      return 1
    fi
    sleep 0.05
  done
}

start_server() {
  local env_root="$1"
  local label="$2"

  mkdir -p "$env_root/logs" "$env_root/runtime"
  # Keep the control socket under a very short path to stay below macOS
  # UNIX-domain socket length limits while leaving the benchmark state isolated.
  local socket_path="$socket_root/${label}.sock"
  local host="unix://${socket_path}"
  local log_path="$env_root/logs/${label}-server.log"

  rm -f "$socket_path"
  (
    cd "$chdir"
    run_cleanroom "$env_root" serve --listen "$host" --gateway-listen "127.0.0.1:0"
  ) >"$log_path" 2>&1 &
  local pid=$!

  wait_for_server "$env_root" "$host" "$pid" "$log_path"
  printf '%s|%s|%s\n' "$pid" "$host" "$log_path"
}

extract_json_string() {
  local file="$1"
  local expr="$2"
  jq -r "$expr // empty" "$file"
}

run_create_iteration() {
  local env_root="$1"
  local label="$2"
  local index="$3"

  local start_output
  start_output="$(start_server "$env_root" "$label")"
  local server_pid="${start_output%%|*}"
  local remainder="${start_output#*|}"
  local host="${remainder%%|*}"
  local server_log="${remainder#*|}"

  mkdir -p "$env_root/logs"
  local create_json="$env_root/logs/${label}-create.json"
  local create_stderr="$env_root/logs/${label}-create.stderr"
  local rm_stdout="$env_root/logs/${label}-rm.stdout"
  local rm_stderr="$env_root/logs/${label}-rm.stderr"
  local create_args=(create --host "$host" -c "$chdir" --json)
  if [[ -n "$backend" ]]; then
    create_args+=(--backend "$backend")
  fi

  local start_ns end_ns elapsed_ns elapsed_seconds sandbox_id observed_backend
  start_ns="$(now_ns)"
  if ! run_cleanroom "$env_root" "${create_args[@]}" >"$create_json" 2>"$create_stderr"; then
    stop_server "$server_pid"
    echo "benchmark create failed for $label" >&2
    echo "server log: $server_log" >&2
    cat "$create_stderr" >&2 || true
    return 1
  fi
  end_ns="$(now_ns)"
  elapsed_ns=$(( end_ns - start_ns ))
  elapsed_seconds="$(awk -v ns="$elapsed_ns" 'BEGIN { printf "%.6f", ns / 1000000000 }')"

  sandbox_id="$(extract_json_string "$create_json" '.sandboxId')"
  if [[ -z "$sandbox_id" ]]; then
    sandbox_id="$(extract_json_string "$create_json" '.sandbox_id')"
  fi
  if [[ -z "$sandbox_id" ]]; then
    stop_server "$server_pid"
    echo "create output missing sandbox id for $label" >&2
    cat "$create_json" >&2 || true
    return 1
  fi

  observed_backend="$(extract_json_string "$create_json" '.backend')"
  if [[ -z "$observed_backend" ]]; then
    observed_backend="unknown"
  fi

  if ! run_cleanroom "$env_root" sandbox rm --host "$host" "$sandbox_id" >"$rm_stdout" 2>"$rm_stderr"; then
    stop_server "$server_pid"
    echo "failed to terminate sandbox $sandbox_id for $label" >&2
    cat "$rm_stderr" >&2 || true
    return 1
  fi

  stop_server "$server_pid"

  jq -n \
    --arg label "$label" \
    --argjson index "$index" \
    --arg sandbox_id "$sandbox_id" \
    --arg backend "$observed_backend" \
    --argjson elapsed_ns "$elapsed_ns" \
    --argjson elapsed_seconds "$elapsed_seconds" \
    --arg host "$host" \
    --arg env_root "$env_root" \
    --arg server_log "$server_log" \
    --arg create_json "$create_json" \
    --arg create_stderr "$create_stderr" \
    '{
      label: $label,
      index: $index,
      sandbox_id: $sandbox_id,
      backend: $backend,
      elapsed_ns: $elapsed_ns,
      elapsed_seconds: $elapsed_seconds,
      host: $host,
      env_root: $env_root,
      logs: {
        server: $server_log,
        create_json: $create_json,
        create_stderr: $create_stderr
      }
    }'
}

warmup_file="$(mktemp "${tmp_root}/warmup.XXXXXX.ndjson")"
runs_file="$(mktemp "${tmp_root}/runs.XXXXXX.ndjson")"
elapsed_file="$(mktemp "${tmp_root}/elapsed.XXXXXX.txt")"

seed_json="null"
shared_env_root="$tmp_root/shared"

case "$scenario" in
  warm-repository-store)
    reset_stage_state "$shared_env_root"
    reset_transport_state "$shared_env_root"
    seed_json="$(run_create_iteration "$shared_env_root" "seed" 0)"
    reset_stage_state "$shared_env_root"
    ;;
  warm-workspace-stage)
    reset_stage_state "$shared_env_root"
    reset_transport_state "$shared_env_root"
    seed_json="$(run_create_iteration "$shared_env_root" "seed" 0)"
    ;;
esac

run_iteration_for_phase() {
  local phase="$1"
  local ordinal="$2"
  local label="${phase}-${ordinal}"
  local env_root

  case "$scenario" in
    cold-host)
      env_root="$tmp_root/${label}"
      reset_stage_state "$env_root"
      reset_transport_state "$env_root"
      ;;
    warm-repository-store)
      env_root="$shared_env_root"
      reset_stage_state "$env_root"
      ;;
    warm-workspace-stage)
      env_root="$shared_env_root"
      ;;
  esac

  run_create_iteration "$env_root" "$label" "$ordinal"
}

echo "Benchmarking repository bootstrap"
echo "- scenario: ${scenario}"
echo "- directory: ${chdir}"
echo "- iterations: ${iterations}"
echo "- warmup: ${warmup}"
echo "- output: ${output_path}"
echo "- temp root: ${tmp_root}"

if [[ "$seed_json" != "null" ]]; then
  echo "- seed: performed"
fi

if [[ "$warmup" -gt 0 ]]; then
  echo "Running warmup iterations"
fi
for i in $(seq 1 "$warmup"); do
  run_json="$(run_iteration_for_phase "warmup" "$i")"
  printf '%s\n' "$run_json" >>"$warmup_file"
done

echo "Running measured iterations"
for i in $(seq 1 "$iterations"); do
  run_json="$(run_iteration_for_phase "run" "$i")"
  printf '%s\n' "$run_json" >>"$runs_file"
  jq -r '.elapsed_seconds' <<<"$run_json" >>"$elapsed_file"
  echo "  run ${i}: $(jq -r '.elapsed_seconds' <<<"$run_json")s"
done

summary_json="$(awk '
  NR == 1 { min = $1; max = $1; sum = 0 }
  { sum += $1; if ($1 < min) min = $1; if ($1 > max) max = $1 }
  END {
    printf "{\"mean\":%.6f,\"min\":%.6f,\"max\":%.6f}", (sum / NR), min, max
  }
' "$elapsed_file")"

jq -n \
  --arg benchmark "repository-bootstrap" \
  --arg timestamp "$timestamp" \
  --arg scenario "$scenario" \
  --arg chdir "$chdir" \
  --arg cleanroom_bin "$cleanroom_bin" \
  --arg backend "$backend" \
  --arg temp_root "$tmp_root" \
  --argjson timeout_seconds "$timeout_seconds" \
  --argjson iterations "$iterations" \
  --argjson warmup "$warmup" \
  --argjson summary "$summary_json" \
  --argjson seed "$seed_json" \
  --slurpfile warmup_runs "$warmup_file" \
  --slurpfile runs "$runs_file" \
  '{
    benchmark: $benchmark,
    timestamp: $timestamp,
    scenario: $scenario,
    config: {
      chdir: $chdir,
      cleanroom_bin: $cleanroom_bin,
      backend: (if $backend == "" then "default" else $backend end),
      iterations: $iterations,
      warmup: $warmup,
      timeout_seconds: $timeout_seconds
    },
    temp_root: $temp_root,
    seed: $seed,
    warmup_runs: $warmup_runs,
    runs: $runs,
    summary: $summary
  }' >"$output_path"

echo "Results written to ${output_path}"
