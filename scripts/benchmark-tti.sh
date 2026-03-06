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
  --backend <name>          Optional backend override for cleanroom exec
  -c, --chdir <path>        Repository/policy directory (default: current directory)
  --output-dir <path>       JSON output directory (default: benchmarks/results)
  --cleanroom-bin <path>    cleanroom binary path (default: cleanroom from PATH, then ./dist/cleanroom)
  -h, --help                Show this help

Environment:
  XDG_RUNTIME_DIR           Used to derive the default unix socket endpoint.

Notes:
  - This script expects the cleanroom server to already be running.
  - The measured command is: cleanroom exec ... -- echo benchmark
  - Sandbox termination runs in hyperfine cleanup and is excluded from timing.
EOF
}

if [[ -n "${XDG_RUNTIME_DIR:-}" ]]; then
  default_host="unix://${XDG_RUNTIME_DIR}/cleanroom/cleanroom.sock"
else
  default_host="unix:///tmp/cleanroom/cleanroom.sock"
fi

if command -v cleanroom >/dev/null 2>&1; then
  cleanroom_bin="$(command -v cleanroom)"
elif [[ -x "./dist/cleanroom" ]]; then
  cleanroom_bin="./dist/cleanroom"
else
  cleanroom_bin="cleanroom"
fi

host="$default_host"
iterations=10
warmup=1
backend=""
chdir="$PWD"
output_dir="benchmarks/results"

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

benchmark_cmd=("$cleanroom_bin" exec --host "$host" -c "$chdir")
if [[ -n "$backend" ]]; then
  benchmark_cmd+=(--backend "$backend")
fi
benchmark_cmd+=(--print-sandbox-id -- echo benchmark)

quoted_benchmark_cmd=""
for token in "${benchmark_cmd[@]}"; do
  printf -v escaped '%q' "$token"
  quoted_benchmark_cmd+="${escaped} "
done
printf -v sandbox_id_escaped '%q' "$sandbox_id_path"
quoted_benchmark_cmd+=" >/dev/null 2>${sandbox_id_escaped}"

printf -v cleanroom_bin_escaped '%q' "$cleanroom_bin"
printf -v host_escaped '%q' "$host"
cleanup_cmd="sid=\$(grep -m1 '^sandbox_id=' ${sandbox_id_escaped} | cut -d= -f2 || true); if [ -n \"\${sid}\" ]; then ${cleanroom_bin_escaped} sandbox rm --host ${host_escaped} \"\${sid}\" >/dev/null 2>&1 || true; fi; : > ${sandbox_id_escaped}"

echo "Benchmarking TTI with hyperfine"
echo "- endpoint: ${host}"
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
rm -f "$sandbox_id_path"

echo "Results written to ${output_path}"
