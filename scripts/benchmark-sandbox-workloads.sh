#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Benchmark sandbox workloads in a single reused sandbox.

Workloads:
  1) Disk small-block write/read throughput + estimated IOPS
  2) Git clone time for a large-ish repository
  3) CPU hashing throughput benchmark

Usage:
  scripts/benchmark-sandbox-workloads.sh [options]

Options:
  --host <endpoint>          Control-plane endpoint (default: unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock)
  --backend <name>           Optional backend override for initial sandbox creation
  -c, --chdir <path>         Repository/policy directory (default: current directory)
  -n, --iterations <count>   Iterations per workload (default: 5)
  --output-dir <path>        Output directory (default: benchmarks/results)
  --cleanroom-bin <path>     cleanroom binary path (default: ./dist/cleanroom, then cleanroom from PATH)
  --repo-url <url>           Git repo for clone benchmark (default: https://github.com/kubernetes/kubernetes.git)
  --repo-depth <count>       Git clone depth (default: 1)
  --iops-block-size <bytes>  Block size for IOPS benchmark (default: 4096)
  --iops-ops <count>         Number of operations for IOPS benchmark (default: 20000)
  --cpu-bytes <bytes>        Bytes hashed per CPU iteration (default: 536870912)
  -h, --help                 Show help

Environment:
  XDG_RUNTIME_DIR            Used to derive the default unix socket endpoint.

Notes:
  - One sandbox is created and reused for all iterations.
  - Sandbox teardown runs after benchmarking and is excluded from measured workload times.
  - The clone benchmark requires `git` in the guest image.
EOF
}

if [[ -n "${XDG_RUNTIME_DIR:-}" ]]; then
  default_host="unix://${XDG_RUNTIME_DIR}/cleanroom/cleanroom.sock"
else
  default_host="unix:///tmp/cleanroom.sock"
fi

if [[ -x "./dist/cleanroom" ]]; then
  cleanroom_bin="./dist/cleanroom"
else
  cleanroom_bin="cleanroom"
fi

host="$default_host"
backend=""
chdir="$PWD"
iterations=5
output_dir="benchmarks/results"
repo_url="https://github.com/kubernetes/kubernetes.git"
repo_depth=1
iops_block_size=4096
iops_ops=20000
cpu_bytes=536870912

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host)
      host="$2"
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
    -n|--iterations)
      iterations="$2"
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
    --repo-url)
      repo_url="$2"
      shift 2
      ;;
    --repo-depth)
      repo_depth="$2"
      shift 2
      ;;
    --iops-block-size)
      iops_block_size="$2"
      shift 2
      ;;
    --iops-ops)
      iops_ops="$2"
      shift 2
      ;;
    --cpu-bytes)
      cpu_bytes="$2"
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
if ! [[ "$repo_depth" =~ ^[0-9]+$ ]] || [[ "$repo_depth" -le 0 ]]; then
  echo "repo-depth must be a positive integer" >&2
  exit 1
fi
if ! [[ "$iops_block_size" =~ ^[0-9]+$ ]] || [[ "$iops_block_size" -le 0 ]]; then
  echo "iops-block-size must be a positive integer" >&2
  exit 1
fi
if ! [[ "$iops_ops" =~ ^[0-9]+$ ]] || [[ "$iops_ops" -le 0 ]]; then
  echo "iops-ops must be a positive integer" >&2
  exit 1
fi
if ! [[ "$cpu_bytes" =~ ^[0-9]+$ ]] || [[ "$cpu_bytes" -le 0 ]]; then
  echo "cpu-bytes must be a positive integer" >&2
  exit 1
fi
if ! command -v "$cleanroom_bin" >/dev/null 2>&1; then
  echo "cleanroom binary not found: $cleanroom_bin" >&2
  exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required but not found in PATH" >&2
  exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required but not found in PATH" >&2
  exit 1
fi

mkdir -p "$output_dir"
timestamp="$(date -u +%Y-%m-%dT%H-%M-%SZ)"
output_path="${output_dir}/${timestamp}-sandbox-workloads.json"

sandbox_id=""

terminate_sandbox() {
  if [[ -z "${sandbox_id}" ]]; then
    return
  fi

  case "$host" in
    unix://*)
      socket_path="${host#unix://}"
      curl -sS --output /dev/null \
        --unix-socket "$socket_path" \
        -H 'Content-Type: application/json' \
        -d "{\"sandbox_id\":\"${sandbox_id}\"}" \
        http://localhost/cleanroom.v1.SandboxService/TerminateSandbox || true
      ;;
    http://*|https://*)
      curl -sS --output /dev/null \
        -H 'Content-Type: application/json' \
        -d "{\"sandbox_id\":\"${sandbox_id}\"}" \
        "${host%/}/cleanroom.v1.SandboxService/TerminateSandbox" || true
      ;;
  esac
}

trap terminate_sandbox EXIT

create_cmd=("$cleanroom_bin" exec --host "$host" -c "$chdir")
if [[ -n "$backend" ]]; then
  create_cmd+=(--backend "$backend")
fi
create_cmd+=(--keep-sandbox -- sh -lc "true")

create_stderr_file="$(mktemp)"
"${create_cmd[@]}" >/dev/null 2>"$create_stderr_file"
sandbox_id="$(sed -n 's/.*sandbox_id=\([^ ]*\).*/\1/p' "$create_stderr_file" | tail -n1)"
rm -f "$create_stderr_file"

if [[ -z "$sandbox_id" ]]; then
  echo "failed to determine sandbox_id from cleanroom exec output" >&2
  exit 1
fi

echo "Benchmarking sandbox workloads"
echo "- endpoint: ${host}"
echo "- sandbox_id: ${sandbox_id}"
echo "- iterations: ${iterations}"
echo "- output: ${output_path}"

run_sandbox_script() {
  local script="$1"
  "$cleanroom_bin" exec --host "$host" -c "$chdir" --sandbox-id "$sandbox_id" -- sh -lc "$script"
}

guest_git_check='command -v git >/dev/null 2>&1 && git --version >/dev/null'
if ! run_sandbox_script "$guest_git_check" >/dev/null 2>&1; then
  echo "guest image is missing git; clone benchmark cannot run" >&2
  exit 1
fi

iops_file="$(mktemp)"
clone_file="$(mktemp)"
cpu_file="$(mktemp)"

for _ in $(seq 1 "$iterations"); do
  iops_script="set -eu; f=/tmp/cleanroom-iops.bin; bs=${iops_block_size}; c=${iops_ops}; ws=\$(awk '{print \$1}' /proc/uptime); dd if=/dev/zero of=\"\$f\" bs=\"\$bs\" count=\"\$c\" conv=fsync >/dev/null 2>&1; we=\$(awk '{print \$1}' /proc/uptime); rs=\$(awk '{print \$1}' /proc/uptime); dd if=\"\$f\" of=/dev/null bs=\"\$bs\" count=\"\$c\" >/dev/null 2>&1; re=\$(awk '{print \$1}' /proc/uptime); rm -f \"\$f\"; awk -v ws=\"\$ws\" -v we=\"\$we\" -v rs=\"\$rs\" -v re=\"\$re\" -v c=\"\$c\" -v bs=\"\$bs\" 'BEGIN { wt=we-ws; rt=re-rs; wi=(wt<=0)?0:(c/wt); ri=(rt<=0)?0:(c/rt); wmb=(wt<=0)?0:((bs*c/1048576)/wt); rmb=(rt<=0)?0:((bs*c/1048576)/rt); printf \"%.6f %.2f %.2f %.6f %.2f %.2f\\n\", wt, wi, wmb, rt, ri, rmb }'"
  run_sandbox_script "$iops_script" >>"$iops_file"

  clone_script="set -eu; d=/tmp/cleanroom-clone-bench; rm -rf \"\$d\"; s=\$(awk '{print \$1}' /proc/uptime); git clone --depth ${repo_depth} ${repo_url@Q} \"\$d\" >/dev/null 2>&1; e=\$(awk '{print \$1}' /proc/uptime); size_mb=\$(du -sm \"\$d\" | awk '{print \$1}'); rm -rf \"\$d\"; awk -v s=\"\$s\" -v e=\"\$e\" -v sz=\"\$size_mb\" 'BEGIN { printf \"%.6f %d\\n\", (e-s), sz }'"
  run_sandbox_script "$clone_script" >>"$clone_file"

  cpu_script="set -eu; bytes=${cpu_bytes}; blocks=\$((bytes/1048576)); if [ \"\$blocks\" -le 0 ]; then blocks=1; fi; s=\$(awk '{print \$1}' /proc/uptime); dd if=/dev/zero bs=1M count=\"\$blocks\" 2>/dev/null | sha256sum >/dev/null; e=\$(awk '{print \$1}' /proc/uptime); awk -v s=\"\$s\" -v e=\"\$e\" -v b=\"\$blocks\" 'BEGIN { t=e-s; mbps=(t<=0)?0:(b/t); printf \"%.6f %.2f\\n\", t, mbps }'"
  run_sandbox_script "$cpu_script" >>"$cpu_file"
done

summarize_1col() {
  local file="$1"
  awk 'NR==1{min=$1;max=$1;sum=0} {sum+=$1; if($1<min) min=$1; if($1>max) max=$1} END {printf "{\"mean\":%.6f,\"min\":%.6f,\"max\":%.6f}", (sum/NR), min, max}' "$file"
}

iops_json="$(awk '
  NR==1 {
    min_wt=$1; max_wt=$1; sum_wt=0;
    min_wi=$2; max_wi=$2; sum_wi=0;
    min_wm=$3; max_wm=$3; sum_wm=0;
    min_rt=$4; max_rt=$4; sum_rt=0;
    min_ri=$5; max_ri=$5; sum_ri=0;
    min_rm=$6; max_rm=$6; sum_rm=0;
  }
  {
    sum_wt+=$1; if($1<min_wt)min_wt=$1; if($1>max_wt)max_wt=$1;
    sum_wi+=$2; if($2<min_wi)min_wi=$2; if($2>max_wi)max_wi=$2;
    sum_wm+=$3; if($3<min_wm)min_wm=$3; if($3>max_wm)max_wm=$3;
    sum_rt+=$4; if($4<min_rt)min_rt=$4; if($4>max_rt)max_rt=$4;
    sum_ri+=$5; if($5<min_ri)min_ri=$5; if($5>max_ri)max_ri=$5;
    sum_rm+=$6; if($6<min_rm)min_rm=$6; if($6>max_rm)max_rm=$6;
  }
  END {
    printf "{\"write\":{\"elapsed_seconds\":{\"mean\":%.6f,\"min\":%.6f,\"max\":%.6f},\"iops\":{\"mean\":%.2f,\"min\":%.2f,\"max\":%.2f},\"throughput_mib_s\":{\"mean\":%.2f,\"min\":%.2f,\"max\":%.2f}},\"read\":{\"elapsed_seconds\":{\"mean\":%.6f,\"min\":%.6f,\"max\":%.6f},\"iops\":{\"mean\":%.2f,\"min\":%.2f,\"max\":%.2f},\"throughput_mib_s\":{\"mean\":%.2f,\"min\":%.2f,\"max\":%.2f}}}",
      (sum_wt/NR), min_wt, max_wt,
      (sum_wi/NR), min_wi, max_wi,
      (sum_wm/NR), min_wm, max_wm,
      (sum_rt/NR), min_rt, max_rt,
      (sum_ri/NR), min_ri, max_ri,
      (sum_rm/NR), min_rm, max_rm;
  }
' "$iops_file")"

clone_elapsed_json="$(summarize_1col "$clone_file")"
clone_size_json="$(awk 'NR==1{min=$2;max=$2;sum=0} {sum+=$2; if($2<min) min=$2; if($2>max) max=$2} END {printf "{\"mean\":%.2f,\"min\":%.0f,\"max\":%.0f}", (sum/NR), min, max}' "$clone_file")"
cpu_elapsed_json="$(summarize_1col "$cpu_file")"
cpu_throughput_json="$(awk 'NR==1{min=$2;max=$2;sum=0} {sum+=$2; if($2<min) min=$2; if($2>max) max=$2} END {printf "{\"mean\":%.2f,\"min\":%.2f,\"max\":%.2f}", (sum/NR), min, max}' "$cpu_file")"

jq -n \
  --arg timestamp "$timestamp" \
  --arg host "$host" \
  --arg sandbox_id "$sandbox_id" \
  --arg backend "${backend:-default}" \
  --arg repo_url "$repo_url" \
  --argjson repo_depth "$repo_depth" \
  --argjson iterations "$iterations" \
  --argjson iops_block_size "$iops_block_size" \
  --argjson iops_ops "$iops_ops" \
  --argjson cpu_bytes "$cpu_bytes" \
  --argjson iops "$iops_json" \
  --argjson clone_elapsed "$clone_elapsed_json" \
  --argjson clone_size "$clone_size_json" \
  --argjson cpu_elapsed "$cpu_elapsed_json" \
  --argjson cpu_throughput "$cpu_throughput_json" \
  '{
    benchmark: "sandbox-workloads",
    timestamp: $timestamp,
    host: $host,
    backend: $backend,
    sandbox_id: $sandbox_id,
    config: {
      iterations: $iterations,
      repo_url: $repo_url,
      repo_depth: $repo_depth,
      iops_block_size_bytes: $iops_block_size,
      iops_operations: $iops_ops,
      cpu_bytes_per_iteration: $cpu_bytes
    },
    results: {
      iops: $iops,
      git_clone: {
        elapsed_seconds: $clone_elapsed,
        cloned_size_mb: $clone_size
      },
      cpu_hash: {
        elapsed_seconds: $cpu_elapsed,
        throughput_mib_s: $cpu_throughput
      }
    }
  }' >"$output_path"

rm -f "$iops_file" "$clone_file" "$cpu_file"

echo "Results written to ${output_path}"
echo
echo "Summary:"
jq -r '
  [
    "IOPS write mean: \(.results.iops.write.iops.mean) ops/s (\(.results.iops.write.throughput_mib_s.mean) MiB/s)",
    "IOPS read mean:  \(.results.iops.read.iops.mean) ops/s (\(.results.iops.read.throughput_mib_s.mean) MiB/s)",
    "Git clone mean:  \(.results.git_clone.elapsed_seconds.mean)s (size \(.results.git_clone.cloned_size_mb.mean) MiB)",
    "CPU hash mean:   \(.results.cpu_hash.throughput_mib_s.mean) MiB/s"
  ] | .[]
' "$output_path"
