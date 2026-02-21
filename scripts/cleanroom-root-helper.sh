#!/usr/bin/env bash
set -euo pipefail

die() {
  echo "cleanroom-root-helper: $*" >&2
  exit 2
}

require_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    die "must run as root"
  fi
}

is_tmp_mount_dir() {
  local p="$1"
  [[ "$p" == /tmp/cleanroom-firecracker-rootfs-* ]]
}

is_runtime_rootfs_tmp() {
  local p="$1"
  [[ "$p" == /var/lib/buildkite-agent/.cache/cleanroom/firecracker/runtime-rootfs/*.ext4.tmp-* ]]
}

is_mounted_rootfs_dest() {
  local p="$1"
  [[ "$p" == /tmp/cleanroom-firecracker-rootfs-*/usr/local/bin/cleanroom-guest-agent || "$p" == /tmp/cleanroom-firecracker-rootfs-*/sbin/cleanroom-init ]]
}

is_tap_name() {
  local v="$1"
  [[ "$v" =~ ^cr[a-z0-9]{1,13}$ ]]
}

is_numeric() {
  local v="$1"
  [[ "$v" =~ ^[0-9]+$ ]]
}

is_ipv4() {
  local v="$1"
  [[ "$v" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]
}

is_cidr() {
  local v="$1"
  [[ "$v" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$ ]]
}

is_install_source() {
  local p="$1"
  [[ "$p" == /var/lib/buildkite-agent/builds/*/buildkite/cleanroom/dist/cleanroom-guest-agent || "$p" == /tmp/cleanroom-init-* ]]
}

run_ip() {
  [[ "$#" -ge 1 ]] || die "ip: missing arguments"
  case "$1" in
    link)
      shift
      if [[ "$#" -eq 1 && "$1" == "show" ]]; then
        exec /usr/sbin/ip link show
      fi
      if [[ "$#" -eq 2 && "$1" == "del" ]]; then
        is_tap_name "$2" || die "ip link del: unsupported interface '$2'"
        exec /usr/sbin/ip link del "$2"
      fi
      if [[ "$#" -eq 4 && "$1" == "set" && "$2" == "dev" && "$4" == "up" ]]; then
        is_tap_name "$3" || die "ip link set: unsupported interface '$3'"
        exec /usr/sbin/ip link set dev "$3" up
      fi
      ;;
    tuntap)
      shift
      if [[ "$#" -eq 7 && "$1" == "add" && "$2" == "dev" && "$4" == "mode" && "$5" == "tap" && "$6" == "user" ]]; then
        is_tap_name "$3" || die "ip tuntap add: unsupported interface '$3'"
        is_numeric "$7" || die "ip tuntap add: invalid uid '$7'"
        exec /usr/sbin/ip tuntap add dev "$3" mode tap user "$7"
      fi
      ;;
    addr)
      shift
      if [[ "$#" -eq 4 && "$1" == "add" && "$3" == "dev" ]]; then
        is_cidr "$2" || die "ip addr add: invalid cidr '$2'"
        is_tap_name "$4" || die "ip addr add: unsupported interface '$4'"
        exec /usr/sbin/ip addr add "$2" dev "$4"
      fi
      ;;
  esac
  die "ip: unsupported arguments"
}

run_iptables() {
  [[ "$#" -ge 1 ]] || die "iptables: missing arguments"

  if [[ "$#" -eq 8 && "$1" == "-t" && "$2" == "nat" && ( "$3" == "-A" || "$3" == "-D" ) && "$4" == "POSTROUTING" && "$5" == "-s" && "$7" == "-j" && "$8" == "MASQUERADE" ]]; then
    is_cidr "$6" || die "iptables nat: invalid cidr '$6'"
    exec /usr/sbin/iptables "$@"
  fi

  if [[ "$#" -eq 10 && ( "$1" == "-A" || "$1" == "-D" ) && "$2" == "FORWARD" && "$3" == "-o" && "$5" == "-m" && "$9" == "-j" && "$10" == "ACCEPT" ]]; then
    is_tap_name "$4" || die "iptables FORWARD -o: unsupported interface '$4'"
    if [[ "$6" == "state" && "$7" == "--state" && "$8" == "RELATED,ESTABLISHED" ]]; then
      exec /usr/sbin/iptables "$@"
    fi
    if [[ "$6" == "conntrack" && "$7" == "--ctstate" && "$8" == "RELATED,ESTABLISHED" ]]; then
      exec /usr/sbin/iptables "$@"
    fi
  fi

  if [[ "$#" -eq 6 && ( "$1" == "-A" || "$1" == "-D" ) && "$2" == "FORWARD" && "$3" == "-i" && "$5" == "-j" && "$6" == "DROP" ]]; then
    is_tap_name "$4" || die "iptables FORWARD drop: unsupported interface '$4'"
    exec /usr/sbin/iptables "$@"
  fi

  if [[ "$#" -eq 12 && ( "$1" == "-A" || "$1" == "-D" ) && "$2" == "FORWARD" && "$3" == "-i" && "$5" == "-p" && ( "$6" == "tcp" || "$6" == "udp" ) && "$7" == "-d" && "$9" == "--dport" && "$11" == "-j" && "$12" == "ACCEPT" ]]; then
    is_tap_name "$4" || die "iptables FORWARD allow: unsupported interface '$4'"
    is_ipv4 "$8" || die "iptables FORWARD allow: invalid destination ip '$8'"
    is_numeric "$10" || die "iptables FORWARD allow: invalid port '$10'"
    exec /usr/sbin/iptables "$@"
  fi

  die "iptables: unsupported arguments"
}

run_sysctl() {
  [[ "$#" -eq 2 ]] || die "sysctl: expected '-w net.ipv4.ip_forward=1'"
  [[ "$1" == "-w" && "$2" == "net.ipv4.ip_forward=1" ]] || die "sysctl: unsupported arguments"
  exec /usr/sbin/sysctl -w net.ipv4.ip_forward=1
}

run_mount() {
  [[ "$#" -eq 4 ]] || die "mount: expected '-o loop <image> <mount-dir>'"
  [[ "$1" == "-o" && "$2" == "loop" ]] || die "mount: unsupported flags"
  is_runtime_rootfs_tmp "$3" || die "mount: unsupported image path"
  is_tmp_mount_dir "$4" || die "mount: unsupported mount path"
  exec /usr/bin/mount -o loop "$3" "$4"
}

run_umount() {
  [[ "$#" -eq 1 ]] || die "umount: expected '<mount-dir>'"
  is_tmp_mount_dir "$1" || die "umount: unsupported mount path"
  exec /usr/bin/umount "$1"
}

run_mkdir() {
  [[ "$#" -eq 3 ]] || die "mkdir: expected '-p <path1> <path2>'"
  [[ "$1" == "-p" ]] || die "mkdir: unsupported arguments"
  [[ "$2" == /tmp/cleanroom-firecracker-rootfs-*/usr/local/bin ]] || die "mkdir: unsupported path '$2'"
  [[ "$3" == /tmp/cleanroom-firecracker-rootfs-*/sbin ]] || die "mkdir: unsupported path '$3'"
  exec /usr/bin/mkdir -p "$2" "$3"
}

run_install() {
  [[ "$#" -eq 4 ]] || die "install: expected '-m 0755 <src> <dest>'"
  [[ "$1" == "-m" && "$2" == "0755" ]] || die "install: unsupported mode"
  local src="$3"
  local dst="$4"
  [[ -f "$src" ]] || die "install: source file not found"
  is_install_source "$src" || die "install: unsupported source path"
  is_mounted_rootfs_dest "$dst" || die "install: unsupported destination path"
  exec /usr/bin/install -m 0755 "$src" "$dst"
}

main() {
  require_root
  [[ "$#" -ge 1 ]] || die "missing command"

  local command="$1"
  shift

  case "$command" in
    true)
      [[ "$#" -eq 0 ]] || die "true: unexpected arguments"
      exec /usr/bin/true
      ;;
    ip)
      run_ip "$@"
      ;;
    iptables)
      run_iptables "$@"
      ;;
    sysctl)
      run_sysctl "$@"
      ;;
    mount)
      run_mount "$@"
      ;;
    umount)
      run_umount "$@"
      ;;
    mkdir)
      run_mkdir "$@"
      ;;
    install)
      run_install "$@"
      ;;
    *)
      die "unsupported command '$command'"
      ;;
  esac
}

main "$@"
