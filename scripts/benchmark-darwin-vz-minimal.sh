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

if [[ -n "${initrd_path}" && -n "${rootfs_path}" ]]; then
  echo "--initrd and --rootfs are mutually exclusive" >&2
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
    --console-log "${console_log}"
  )
  if [[ -n "${boot_args}" ]]; then
    runner_args+=(--boot-args "${boot_args}")
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
