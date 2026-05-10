#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Benchmark the minimal darwin-vz boot path.

Usage:
  scripts/benchmark-darwin-vz-minimal.sh --kernel <Image> [options]
  scripts/benchmark-darwin-vz-minimal.sh --build-kernel [options]

Options:
  --kernel <path>           Linux kernel Image to boot.
  --build-kernel            Build the minimal benchmark kernel with Docker before running.
  --kernel-profile <name>   Kernel profile for --build-kernel: initrd or rootfs (default: initrd, or rootfs with --rootfs). Must match the selected boot medium.
  --initrd <path>           Initrd to boot. If omitted and --rootfs is omitted, the minimal initrd is built.
  --rootfs <path>           Writable rootfs image to boot instead of initrd.
  -n, --iterations <count>  Number of runs (default: 10).
  --vcpus <count>           Guest vCPU count (default: 2).
  --memory-mib <mib>        Guest memory in MiB (default: 1024).
  --probe <name>            Probe to run: exec or memory-reporting (default: exec).
  --probe-memory-mib <mib>  Memory touched by memory-reporting probe (default: 256).
  --probe-pre-touch-ms <ms> Delay after the before sample (default: 500).
  --probe-hold-ms <ms>      Delay after touching memory (default: 1000).
  --probe-post-free-ms <ms> Delay after freeing memory (default: 3000).
  --balloon-device <on|off> Attach the VZ virtio memory balloon (default: on).
  --initial-balloon-target-mib <mib>
                            Set VZ balloon target before VM start.
  --pre-probe-balloon-target-mib <mib>
                            Set VZ balloon target before running the probe.
  --pre-probe-balloon-settle-ms <ms>
                            Delay after pre-probe balloon target (default: 1000).
  --balloon-target-mib <mib>
                            Set explicit VZ balloon target after the guest frees memory.
  --boot-args <args>        Full Linux command line override.
  --output-dir <path>       JSONL output directory (default: benchmarks/results).
  -h, --help                Show this help.

This is a lower-bound Virtualization.framework probe, not a Cleanroom TTI benchmark.
EOF
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
MINIMAL_DIR="${REPO_ROOT}/benchmarks/darwin-vz/minimal"

kernel_path=""
initrd_path=""
rootfs_path=""
boot_args=""
kernel_profile=""
iterations=10
vcpus=2
memory_mib=1024
probe="exec"
probe_memory_mib=256
probe_pre_touch_ms=500
probe_hold_ms=1000
probe_post_free_ms=3000
balloon_device="on"
initial_balloon_target_mib=""
pre_probe_balloon_target_mib=""
pre_probe_balloon_settle_ms=1000
balloon_target_mib=""
output_dir="${REPO_ROOT}/benchmarks/results"
build_kernel=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --kernel)
      kernel_path="$2"
      shift 2
      ;;
    --build-kernel)
      build_kernel=1
      shift
      ;;
    --kernel-profile)
      kernel_profile="$2"
      shift 2
      ;;
    --initrd)
      initrd_path="$2"
      shift 2
      ;;
    --rootfs)
      rootfs_path="$2"
      shift 2
      ;;
    -n|--iterations)
      iterations="$2"
      shift 2
      ;;
    --vcpus)
      vcpus="$2"
      shift 2
      ;;
    --memory-mib)
      memory_mib="$2"
      shift 2
      ;;
    --probe)
      probe="$2"
      shift 2
      ;;
    --probe-memory-mib)
      probe_memory_mib="$2"
      shift 2
      ;;
    --probe-pre-touch-ms)
      probe_pre_touch_ms="$2"
      shift 2
      ;;
    --probe-hold-ms)
      probe_hold_ms="$2"
      shift 2
      ;;
    --probe-post-free-ms)
      probe_post_free_ms="$2"
      shift 2
      ;;
    --balloon-device)
      balloon_device="$2"
      shift 2
      ;;
    --initial-balloon-target-mib)
      initial_balloon_target_mib="$2"
      shift 2
      ;;
    --pre-probe-balloon-target-mib)
      pre_probe_balloon_target_mib="$2"
      shift 2
      ;;
    --pre-probe-balloon-settle-ms)
      pre_probe_balloon_settle_ms="$2"
      shift 2
      ;;
    --balloon-target-mib)
      balloon_target_mib="$2"
      shift 2
      ;;
    --boot-args)
      boot_args="$2"
      shift 2
      ;;
    --output-dir)
      output_dir="$2"
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

require_positive_int() {
  local name="$1"
  local value="$2"
  if ! [[ "${value}" =~ ^[0-9]+$ ]] || [[ "${value}" -le 0 ]]; then
    echo "${name} must be a positive integer" >&2
    exit 1
  fi
}

run_repo_tool() {
  if command -v mise >/dev/null 2>&1; then
    mise exec -- "$@"
  else
    "$@"
  fi
}

require_positive_int "iterations" "${iterations}"
require_positive_int "vcpus" "${vcpus}"
require_positive_int "memory-mib" "${memory_mib}"
require_positive_int "probe-memory-mib" "${probe_memory_mib}"
require_positive_int "probe-pre-touch-ms" "${probe_pre_touch_ms}"
require_positive_int "probe-hold-ms" "${probe_hold_ms}"
require_positive_int "probe-post-free-ms" "${probe_post_free_ms}"
require_positive_int "pre-probe-balloon-settle-ms" "${pre_probe_balloon_settle_ms}"

case "${probe}" in
  exec|memory-reporting) ;;
  *)
    echo "unsupported --probe: ${probe}" >&2
    exit 1
    ;;
esac
case "${balloon_device}" in
  on|off) ;;
  *)
    echo "unsupported --balloon-device: ${balloon_device}" >&2
    exit 1
    ;;
esac
if [[ -n "${balloon_target_mib}" ]]; then
  require_positive_int "balloon-target-mib" "${balloon_target_mib}"
fi
if [[ -n "${initial_balloon_target_mib}" ]]; then
  require_positive_int "initial-balloon-target-mib" "${initial_balloon_target_mib}"
fi
if [[ -n "${pre_probe_balloon_target_mib}" ]]; then
  require_positive_int "pre-probe-balloon-target-mib" "${pre_probe_balloon_target_mib}"
fi
if [[ "${probe}" == "memory-reporting" && "${probe_memory_mib}" -ge "${memory_mib}" ]]; then
  echo "--probe-memory-mib must be smaller than --memory-mib" >&2
  exit 1
fi
if [[ -n "${balloon_target_mib}" && "${balloon_target_mib}" -gt "${memory_mib}" ]]; then
  echo "--balloon-target-mib must be less than or equal to --memory-mib" >&2
  exit 1
fi
if [[ -n "${initial_balloon_target_mib}" && "${initial_balloon_target_mib}" -gt "${memory_mib}" ]]; then
  echo "--initial-balloon-target-mib must be less than or equal to --memory-mib" >&2
  exit 1
fi
if [[ -n "${pre_probe_balloon_target_mib}" && "${pre_probe_balloon_target_mib}" -gt "${memory_mib}" ]]; then
  echo "--pre-probe-balloon-target-mib must be less than or equal to --memory-mib" >&2
  exit 1
fi
if [[ "${balloon_device}" == "off" && (-n "${balloon_target_mib}" || -n "${initial_balloon_target_mib}" || -n "${pre_probe_balloon_target_mib}") ]]; then
  echo "balloon target options require --balloon-device on" >&2
  exit 1
fi
if [[ -n "${balloon_target_mib}" && "${probe}" != "memory-reporting" ]]; then
  echo "--balloon-target-mib requires --probe memory-reporting" >&2
  exit 1
fi

if [[ -n "${initrd_path}" && -n "${rootfs_path}" ]]; then
  echo "--initrd and --rootfs are mutually exclusive" >&2
  exit 1
fi
if [[ "${probe}" == "memory-reporting" && -n "${rootfs_path}" ]]; then
  echo "--probe memory-reporting currently requires initrd mode" >&2
  exit 1
fi

if [[ -z "${kernel_profile}" ]]; then
  if [[ -n "${rootfs_path}" ]]; then
    kernel_profile="rootfs"
  else
    kernel_profile="initrd"
  fi
fi
case "${kernel_profile}" in
  initrd|rootfs) ;;
  *)
    echo "unsupported --kernel-profile: ${kernel_profile}" >&2
    exit 1
    ;;
esac

if [[ "${build_kernel}" -eq 1 && -n "${kernel_path}" ]]; then
  echo "--kernel and --build-kernel are mutually exclusive" >&2
  exit 1
fi
if [[ "${build_kernel}" -eq 0 && -z "${kernel_path}" ]]; then
  echo "missing --kernel <path> or --build-kernel" >&2
  usage >&2
  exit 1
fi
if [[ "${build_kernel}" -eq 1 ]]; then
  expected_kernel_profile="initrd"
  if [[ -n "${rootfs_path}" ]]; then
    expected_kernel_profile="rootfs"
  fi
  if [[ "${kernel_profile}" != "${expected_kernel_profile}" ]]; then
    echo "--kernel-profile ${kernel_profile} does not match the selected boot medium; expected ${expected_kernel_profile}" >&2
    exit 1
  fi
fi

runner_path="${REPO_ROOT}/dist/darwin-vz-minimal"
run_repo_tool "${MINIMAL_DIR}/build-runner.sh" "${runner_path}" >/dev/null

if [[ -z "${rootfs_path}" && -z "${initrd_path}" ]]; then
  initrd_path="${REPO_ROOT}/dist/darwin-vz-minimal-initrd.cpio.gz"
  run_repo_tool "${MINIMAL_DIR}/build-initrd.sh" "${initrd_path}" >/dev/null
fi

if [[ "${build_kernel}" -eq 1 ]]; then
  kernel_arch="${CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_ARCH:-arm64}"
  kernel_version="${CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_VERSION:-6.1.155}"
  kernel_path="${REPO_ROOT}/dist/darwin-vz-minimal-${kernel_profile}-${kernel_arch}-kernel-${kernel_version}-Image"
  CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_PROFILE="${kernel_profile}" \
    "${MINIMAL_DIR}/build-kernel.sh" "${kernel_path}" >/dev/null
fi

if [[ ! -r "${kernel_path}" ]]; then
  echo "kernel not found or unreadable: ${kernel_path}" >&2
  exit 1
fi
if [[ -n "${initrd_path}" && ! -r "${initrd_path}" ]]; then
  echo "initrd not found or unreadable: ${initrd_path}" >&2
  exit 1
fi
if [[ -n "${rootfs_path}" && ! -r "${rootfs_path}" ]]; then
  echo "rootfs not found or unreadable: ${rootfs_path}" >&2
  exit 1
fi

mkdir -p "${output_dir}"
timestamp="$(date -u +%Y-%m-%dT%H-%M-%SZ)"
output_path="${output_dir}/${timestamp}-darwin-vz-minimal.jsonl"
console_dir="${output_dir}/${timestamp}-darwin-vz-minimal-console"
mkdir -p "${console_dir}"

mode_args=()
if [[ -n "${rootfs_path}" ]]; then
  mode_args=(--rootfs "${rootfs_path}")
else
  mode_args=(--initrd "${initrd_path}")
fi

for i in $(seq 1 "${iterations}"); do
  console_log="${console_dir}/run-${i}.log"
  runner_args=(
    --kernel "${kernel_path}"
    "${mode_args[@]}"
    --vcpus "${vcpus}"
    --memory-mib "${memory_mib}"
    --probe "${probe}"
    --probe-memory-mib "${probe_memory_mib}"
    --probe-pre-touch-ms "${probe_pre_touch_ms}"
    --probe-hold-ms "${probe_hold_ms}"
    --probe-post-free-ms "${probe_post_free_ms}"
    --balloon-device "${balloon_device}"
    --console-log "${console_log}"
  )
  if [[ -n "${boot_args}" ]]; then
    runner_args+=(--boot-args "${boot_args}")
  fi
  if [[ -n "${initial_balloon_target_mib}" ]]; then
    runner_args+=(--initial-balloon-target-mib "${initial_balloon_target_mib}")
  fi
  if [[ -n "${pre_probe_balloon_target_mib}" ]]; then
    runner_args+=(--pre-probe-balloon-target-mib "${pre_probe_balloon_target_mib}")
    runner_args+=(--pre-probe-balloon-settle-ms "${pre_probe_balloon_settle_ms}")
  fi
  if [[ -n "${balloon_target_mib}" ]]; then
    runner_args+=(--balloon-target-mib "${balloon_target_mib}")
  fi

  "${runner_path}" "${runner_args[@]}" | tee -a "${output_path}"
done

printf 'wrote %s\n' "${output_path}"

if command -v jq >/dev/null 2>&1; then
  jq -s -r '
    def round1: (. * 10 | round / 10);
    def stat($xs): ($xs | sort) as $s |
      "n=" + (($s|length)|tostring) +
      " median=" + ((if ($s|length)%2==1 then $s[(($s|length)/2|floor)] else (($s[(($s|length)/2)-1] + $s[(($s|length)/2)]) / 2) end)|round1|tostring) +
      " mean=" + (($s|add/length)|round1|tostring) +
      " min=" + ($s[0]|round1|tostring) +
      " max=" + ($s[-1]|round1|tostring);
    "exec_response_ms " + stat(map(.exec_response_ms)) + "\n" +
    "vsock_connect_ms " + stat(map(.vsock_connect_ms))
  ' "${output_path}"
fi
