#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

KERNEL_VERSION="${CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_VERSION:-6.1.155}"
KERNEL_PROFILE="${CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_PROFILE:-initrd}"
KERNEL_ARCH="${CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_ARCH:-arm64}"
DOCKER_IMAGE="${CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_DOCKER_IMAGE:-ubuntu:22.04}"
DOCKER_PLATFORM="${CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_DOCKER_PLATFORM:-linux/${KERNEL_ARCH}}"
BUILD_VOLUME="${CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_BUILD_VOLUME:-cleanroom-darwin-vz-minimal-kernel}"
DEFAULT_KERNEL_TARBALL_SHA256=""
if [[ "${KERNEL_VERSION}" == "6.1.155" ]]; then
  DEFAULT_KERNEL_TARBALL_SHA256="c29387aeee085fbcbd91236224b9df805063bac43615e75cea2c6b29604a5c73"
fi
KERNEL_TARBALL_SHA256="${CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_TARBALL_SHA256:-${DEFAULT_KERNEL_TARBALL_SHA256}}"

case "${KERNEL_ARCH}" in
  arm64) ;;
  *)
    echo "unsupported CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_ARCH: ${KERNEL_ARCH}" >&2
    echo "only arm64 is currently supported" >&2
    exit 1
    ;;
esac

if [[ -z "${KERNEL_TARBALL_SHA256}" ]]; then
  echo "missing CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_TARBALL_SHA256 for Linux ${KERNEL_VERSION}" >&2
  exit 1
fi

case "${KERNEL_PROFILE}" in
  initrd|rootfs) ;;
  *)
    echo "unsupported CLEANROOM_DARWIN_VZ_MINIMAL_KERNEL_PROFILE: ${KERNEL_PROFILE}" >&2
    echo "expected initrd or rootfs" >&2
    exit 1
    ;;
esac

OUTPUT_PATH="${1:-${REPO_ROOT}/dist/darwin-vz-minimal-${KERNEL_PROFILE}-${KERNEL_ARCH}-kernel-${KERNEL_VERSION}-Image}"
CONFIG_PATH="${OUTPUT_PATH}.config"
mkdir -p "$(dirname "${OUTPUT_PATH}")"
OUTPUT_DIR="$(cd "$(dirname "${OUTPUT_PATH}")" && pwd)"
OUTPUT_BASENAME="$(basename "${OUTPUT_PATH}")"
CONFIG_BASENAME="$(basename "${CONFIG_PATH}")"

docker run --rm \
  --platform "${DOCKER_PLATFORM}" \
  -e "KERNEL_VERSION=${KERNEL_VERSION}" \
  -e "KERNEL_PROFILE=${KERNEL_PROFILE}" \
  -e "KERNEL_ARCH=${KERNEL_ARCH}" \
  -e "KERNEL_TARBALL_SHA256=${KERNEL_TARBALL_SHA256}" \
  -e "OUTPUT_BASENAME=${OUTPUT_BASENAME}" \
  -e "CONFIG_BASENAME=${CONFIG_BASENAME}" \
  -v "${BUILD_VOLUME}:/build" \
  -v "${OUTPUT_DIR}:/out" \
  "${DOCKER_IMAGE}" \
  bash -lc '
    set -euo pipefail

    export DEBIAN_FRONTEND=noninteractive
    apt-get update
    apt-get install -y --no-install-recommends \
      bc \
      bison \
      build-essential \
      ca-certificates \
      curl \
      flex \
      libelf-dev \
      libssl-dev \
      xz-utils

    src="/build/linux-${KERNEL_VERSION}"
    tarball="/build/linux-${KERNEL_VERSION}.tar.xz"
    source_stamp="${src}.source.sha256"

    download_kernel_tarball() {
      rm -f "${tarball}.tmp"
      curl -fsSL \
        "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz" \
        -o "${tarball}.tmp"
      mv "${tarball}.tmp" "${tarball}"
    }

    verify_kernel_tarball() {
      echo "${KERNEL_TARBALL_SHA256}  ${tarball}" | sha256sum -c -
    }

    if [[ ! -d "${src}" || ! -f "${source_stamp}" || "$(<"${source_stamp}")" != "${KERNEL_TARBALL_SHA256}" ]]; then
      rm -rf "${src}" "${source_stamp}"
      if [[ ! -f "${tarball}" ]]; then
        download_kernel_tarball
      fi
      if ! verify_kernel_tarball; then
        rm -f "${tarball}"
        download_kernel_tarball
        verify_kernel_tarball
      fi
      tar -C /build -xf "${tarball}"
      printf "%s\n" "${KERNEL_TARBALL_SHA256}" > "${source_stamp}"
    fi

    out="/build/out-vz-${KERNEL_PROFILE}-pci-${KERNEL_VERSION}"
    rm -rf "${out}"
    mkdir -p "${out}"

    make -C "${src}" O="${out}" ARCH="${KERNEL_ARCH}" tinyconfig
    "${src}/scripts/config" --file "${out}/.config" \
      -e 64BIT \
      -e ARM64_4K_PAGES \
      -e BINFMT_ELF \
      -e TTY \
      -e VT \
      -e UNIX98_PTYS \
      -e HVC_DRIVER \
      -e VIRTIO \
      -e VIRTIO_MENU \
      -e VIRTIO_PCI \
      -e VIRTIO_MMIO \
      -e VIRTIO_PCI_LEGACY \
      -e VIRTIO_CONSOLE \
      -e VSOCKETS \
      -e VIRTIO_VSOCKETS \
      -e NET \
      -e INET \
      -e PROC_FS \
      -e SYSFS \
      -e FUTEX \
      -e EPOLL \
      -e ANON_INODES \
      -e EVENTFD \
      -e SIGNALFD \
      -e TIMERFD \
      -e PRINTK \
      -e BUG \
      -e DEVTMPFS \
      -e TMPFS \
      -e EMBEDDED \
      -e EXPERT \
      -e PCI \
      -e PCI_HOST_GENERIC \
      -e PCI_MSI \
      -e PCI_MSI_IRQ_DOMAIN \
      -e GENERIC_MSI_IRQ_DOMAIN \
      -e CC_OPTIMIZE_FOR_PERFORMANCE \
      -d CC_OPTIMIZE_FOR_SIZE \
      -d MODULES \
      -d DEBUG_INFO \
      -d DEBUG_KERNEL \
      -d KALLSYMS \
      -d IKCONFIG \
      -d IPV6 \
      -d WIRELESS \
      -d WLAN \
      -d BT \
      -d BLOCK \
      -d SCSI \
      -d MD \
      -d INPUT \
      -d HID \
      -d USB_SUPPORT \
      -d CGROUPS \
      -d NAMESPACES \
      -d AUDIT \
      -d SECURITY \
      -d BPF_SYSCALL \
      -d ZSWAP

    if [[ "${KERNEL_PROFILE}" = "initrd" ]]; then
      "${src}/scripts/config" --file "${out}/.config" \
        -e BLK_DEV_INITRD \
        -e RD_GZIP \
        -e CMDLINE_BOOL \
        --set-str CMDLINE "rdinit=/init"
    fi

    if [[ "${KERNEL_PROFILE}" = "rootfs" ]]; then
      "${src}/scripts/config" --file "${out}/.config" \
        -e BLOCK \
        -e BLK_DEV \
        -e VIRTIO_BLK \
        -e EXT4_FS \
        -e EXT4_USE_FOR_EXT2 \
        -e JBD2 \
        -e CRC16 \
        -e CRYPTO \
        -e CRYPTO_CRC32C \
        -e NETDEVICES \
        -e VIRTIO_NET \
        -e PACKET \
        -e UNIX \
        -e DEVTMPFS_MOUNT
    fi

    make -C "${src}" O="${out}" ARCH="${KERNEL_ARCH}" olddefconfig
    make -C "${src}" O="${out}" ARCH="${KERNEL_ARCH}" -j"$(nproc)" Image

    cp "${out}/arch/${KERNEL_ARCH}/boot/Image" "/out/${OUTPUT_BASENAME}"
    cp "${out}/.config" "/out/${CONFIG_BASENAME}"
  '

printf '%s\n' "${OUTPUT_PATH}"
