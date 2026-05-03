#!/usr/bin/env bash
set -euo pipefail

# Security model:
# - This script is a root-owned allowlist for privileged cleanroom operations.
# - Unprivileged callers reach it via `sudo -n /usr/local/sbin/cleanroom-root-helper ...`.
# - Roll it out from trusted host administration paths such as bootstrap or SSM, not PR CI.
#
# Sharp edges:
# - Every new command, flag shape, or wildcard expands the root execution surface.
# - Keep validation explicit; do not add generic passthroughs, shell evaluation, or broad writable paths.
# - Helper updates must land on hosts before branches that depend on new capabilities can pass.

helper_contract_version() {
  echo "9"
}

helper_has_zfs() {
  [[ -x /usr/sbin/zfs || -x /sbin/zfs ]] && return 0
  command -v zfs >/dev/null 2>&1
}

helper_capabilities() {
  cat <<'EOF'
firecracker-network
firecracker-trusted-dns
firecracker-nflog
EOF

  if helper_has_zfs; then
    echo "firecracker-zfs"
    echo "firecracker-zfs-metadata"
    echo "firecracker-zfs-transfer"
  fi
}

die() {
  echo "cleanroom-root-helper: $*" >&2
  exit 2
}

require_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    die "must run as root"
  fi
}

is_runtime_rootfs_image() {
  local p="$1"
  [[ "$p" == */cleanroom/firecracker/runtime-rootfs/*.ext4 ]]
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

is_trusted_dns_set_name() {
  local v="$1"
  [[ "$v" =~ ^crdns-(tcp|udp)-cr[a-z0-9]{1,13}$ ]]
}

is_trusted_dns_chain_name() {
  local v="$1"
  [[ "$v" =~ ^crdns-(tcp|udp)-cr[a-z0-9]{1,13}$ ]]
}

is_ipset_entry() {
  local v="$1"
  [[ "$v" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3},(tcp|udp):[0-9]+$ ]]
}

is_zfs_dataset() {
  local v="$1"
  [[ "$v" =~ ^[A-Za-z0-9][A-Za-z0-9._:-]*(/[A-Za-z0-9][A-Za-z0-9._:-]*)*$ ]]
}

is_zfs_snapshot_ref() {
  local v="$1"
  [[ "$v" =~ ^[A-Za-z0-9][A-Za-z0-9._:-]*(/[A-Za-z0-9][A-Za-z0-9._:-]*)*@[A-Za-z0-9][A-Za-z0-9._:-]*$ ]]
}

contains_cleanroom_zfs_namespace() {
  local ref="$1"
  local dataset="${ref%@*}"
  local IFS='/'
  read -r -a components <<<"$dataset"
  local component_count="${#components[@]}"
  local i

  # Only allow descendants of the cleanroom namespace, not the namespace root.
  for ((i = 0; i < component_count - 1; i++)); do
    if [[ "${components[$i]}" == "cleanroom" ]]; then
      return 0
    fi
  done
  return 1
}

is_cleanroom_zfs_dataset() {
  local v="$1"
  is_zfs_dataset "$v" || return 1
  contains_cleanroom_zfs_namespace "$v"
}

is_cleanroom_zfs_snapshot_ref() {
  local v="$1"
  is_zfs_snapshot_ref "$v" || return 1
  contains_cleanroom_zfs_namespace "$v"
}

is_cleanroom_zfs_stored_snapshot_ref() {
  local v="$1"
  is_cleanroom_zfs_snapshot_ref "$v" || return 1

  local dataset="${v%@*}"
  local snapshot_name="${v##*@}"
  [[ "$snapshot_name" == "base" ]] || return 1

  local IFS='/'
  read -r -a components <<<"$dataset"
  local component_count="${#components[@]}"
  local i

  for ((i = 0; i <= component_count - 3; i++)); do
    if [[ "${components[$i]}" == "cleanroom" && "${components[$((i + 1))]}" == "snapshots" && "${components[$((i + 2))]}" != "imports" && $((component_count - i)) -eq 3 ]]; then
      return 0
    fi
    if [[ "${components[$i]}" == "cleanroom" && "${components[$((i + 1))]}" == "snapshots" && "${components[$((i + 2))]}" == "imports" && $((component_count - i)) -eq 4 ]]; then
      return 0
    fi
  done
  return 1
}

is_cleanroom_zfs_snapshot_import_dataset() {
  local v="$1"
  is_cleanroom_zfs_dataset "$v" || return 1

  local IFS='/'
  read -r -a components <<<"$v"
  local component_count="${#components[@]}"
  local i

  for ((i = 0; i <= component_count - 4; i++)); do
    if [[ "${components[$i]}" == "cleanroom" && "${components[$((i + 1))]}" == "snapshots" && "${components[$((i + 2))]}" == "imports" && $((component_count - i)) -eq 4 ]]; then
      return 0
    fi
  done
  return 1
}

is_cleanroom_zfs_snapshot_import_namespace_dataset() {
  local v="$1"
  is_cleanroom_zfs_dataset "$v" || return 1

  local IFS='/'
  read -r -a components <<<"$v"
  local component_count="${#components[@]}"
  local i

  for ((i = 0; i <= component_count - 3; i++)); do
    if [[ "${components[$i]}" == "cleanroom" && "${components[$((i + 1))]}" == "snapshots" && "${components[$((i + 2))]}" == "imports" && $((component_count - i)) -eq 3 ]]; then
      return 0
    fi
  done
  return 1
}

is_zvol_device_path() {
  local p="$1"
  [[ "$p" == /dev/zvol/* ]] || return 1
  is_cleanroom_zfs_dataset "${p#/dev/zvol/}"
}

zvol_device_path_for_dataset() {
  local dataset="$1"
  printf '/dev/zvol/%s\n' "${dataset#/}"
}

wait_for_zvol_device_path() {
  local dataset="$1"
  local path

  path="$(zvol_device_path_for_dataset "$dataset")"
  for _ in {1..50}; do
    if [[ -e "$path" ]]; then
      return 0
    fi
    sleep 0.1
  done
  die "zfs: timed out waiting for zvol device '$path'"
}

zfs_bin() {
  if [[ -x /usr/sbin/zfs ]]; then
    echo /usr/sbin/zfs
    return
  fi
  if [[ -x /sbin/zfs ]]; then
    echo /sbin/zfs
    return
  fi
  command -v zfs || die "zfs: binary not found"
}

dd_bin() {
  if [[ -x /usr/bin/dd ]]; then
    echo /usr/bin/dd
    return
  fi
  if [[ -x /bin/dd ]]; then
    echo /bin/dd
    return
  fi
  command -v dd || die "dd: binary not found"
}

ipset_bin() {
  if [[ -x /usr/sbin/ipset ]]; then
    echo /usr/sbin/ipset
    return
  fi
  if [[ -x /sbin/ipset ]]; then
    echo /sbin/ipset
    return
  fi
  command -v ipset || die "ipset: binary not found"
}

run_ip() {
  [[ "$#" -ge 1 ]] || die "ip: missing arguments"
  case "$1" in
    -o)
      shift
      if [[ "$#" -eq 2 && "$1" == "link" && "$2" == "show" ]]; then
        exec /usr/sbin/ip -o link show
      fi
      ;;
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

  # List rules: iptables -S <chain>
  if [[ "$#" -eq 2 && "$1" == "-S" && ( "$2" == "INPUT" || "$2" == "FORWARD" ) ]]; then
    exec /usr/sbin/iptables "$@"
  fi

  # List NAT rules: iptables -t nat -S POSTROUTING
  if [[ "$#" -eq 4 && "$1" == "-t" && "$2" == "nat" && "$3" == "-S" && "$4" == "POSTROUTING" ]]; then
    exec /usr/sbin/iptables "$@"
  fi

  if [[ "$#" -eq 8 && "$1" == "-t" && "$2" == "nat" && ( "$3" == "-A" || "$3" == "-D" ) && "$4" == "POSTROUTING" && "$5" == "-s" && "$7" == "-j" && "$8" == "MASQUERADE" ]]; then
    is_cidr "$6" || die "iptables nat: invalid cidr '$6'"
    exec /usr/sbin/iptables "$@"
  fi

  if [[ "$#" -eq 14 && "$1" == "-t" && "$2" == "nat" && ( "$3" == "-A" || "$3" == "-D" ) && "$4" == "PREROUTING" && "$5" == "-i" && "$7" == "-p" && ( "$8" == "tcp" || "$8" == "udp" ) && "$9" == "--dport" && "${10}" == "53" && "${11}" == "-j" && "${12}" == "REDIRECT" && "${13}" == "--to-ports" ]]; then
    is_tap_name "$6" || die "iptables PREROUTING redirect: unsupported interface '$6'"
    is_numeric "${14}" || die "iptables PREROUTING redirect: invalid port '${14}'"
    exec /usr/sbin/iptables "$@"
  fi

  if [[ "$#" -eq 10 && ( "$1" == "-A" || "$1" == "-D" ) && "$2" == "FORWARD" && ( "$3" == "-o" || "$3" == "-i" ) && "$5" == "-m" && "$9" == "-j" && "${10}" == "ACCEPT" ]]; then
    is_tap_name "$4" || die "iptables FORWARD ${3}: unsupported interface '$4'"
    if [[ "$6" == "state" && "$7" == "--state" && "$8" == "RELATED,ESTABLISHED" ]]; then
      exec /usr/sbin/iptables "$@"
    fi
    if [[ "$6" == "conntrack" && "$7" == "--ctstate" && "$8" == "RELATED,ESTABLISHED" ]]; then
      exec /usr/sbin/iptables "$@"
    fi
  fi

  if [[ "$#" -eq 6 && ( "$1" == "-A" || "$1" == "-D" ) && ( "$2" == "FORWARD" || "$2" == "INPUT" ) && "$3" == "-i" && "$5" == "-j" && "$6" == "DROP" ]]; then
    is_tap_name "$4" || die "iptables $2 drop: unsupported interface '$4'"
    exec /usr/sbin/iptables "$@"
  fi

  if [[ "$#" -eq 13 && ( "$1" == "-A" || "$1" == "-D" ) && "$2" == "FORWARD" && "$3" == "-i" && "$5" == "-p" && ( "$6" == "tcp" || "$6" == "udp" ) && "$7" == "-m" && "$8" == "set" && "$9" == "--match-set" && "${11}" == "dst,dst" && "${12}" == "-j" && "${13}" == "ACCEPT" ]]; then
    is_tap_name "$4" || die "iptables FORWARD set allow: unsupported interface '$4'"
    is_trusted_dns_set_name "${10}" || die "iptables FORWARD set allow: unsupported set '${10}'"
    exec /usr/sbin/iptables "$@"
  fi

  if [[ "$#" -eq 2 && "$1" == "-N" ]]; then
    is_trusted_dns_chain_name "$2" || die "iptables chain create: unsupported chain '$2'"
    exec /usr/sbin/iptables "$@"
  fi

  if [[ "$#" -eq 2 && ( "$1" == "-F" || "$1" == "-X" ) ]]; then
    is_trusted_dns_chain_name "$2" || die "iptables chain $1: unsupported chain '$2'"
    exec /usr/sbin/iptables "$@"
  fi

  if [[ "$#" -eq 8 && ( "$1" == "-A" || "$1" == "-D" ) && "$2" == "FORWARD" && "$3" == "-i" && "$5" == "-p" && ( "$6" == "tcp" || "$6" == "udp" ) && "$7" == "-j" ]]; then
    is_tap_name "$4" || die "iptables FORWARD chain jump: unsupported interface '$4'"
    is_trusted_dns_chain_name "$8" || die "iptables FORWARD chain jump: unsupported chain '$8'"
    exec /usr/sbin/iptables "$@"
  fi

  if [[ "$#" -eq 10 && ( "$1" == "-A" || "$1" == "-D" ) && "$3" == "-d" && "$5" == "-p" && ( "$6" == "tcp" || "$6" == "udp" ) && "$7" == "--dport" && "$9" == "-j" && "${10}" == "ACCEPT" ]]; then
    is_trusted_dns_chain_name "$2" || die "iptables trusted dns allow: unsupported chain '$2'"
    is_ipv4 "$4" || die "iptables trusted dns allow: invalid destination ip '$4'"
    is_numeric "$8" || die "iptables trusted dns allow: invalid port '$8'"
    exec /usr/sbin/iptables "$@"
  fi

  if [[ "$#" -eq 12 && ( "$1" == "-A" || "$1" == "-D" ) && "$2" == "FORWARD" && "$3" == "-i" && "$5" == "-p" && ( "$6" == "tcp" || "$6" == "udp" ) && "$7" == "-d" && "$9" == "--dport" && "${11}" == "-j" && "${12}" == "ACCEPT" ]]; then
    is_tap_name "$4" || die "iptables FORWARD allow: unsupported interface '$4'"
    is_ipv4 "$8" || die "iptables FORWARD allow: invalid destination ip '$8'"
    is_numeric "${10}" || die "iptables FORWARD allow: invalid port '${10}'"
    exec /usr/sbin/iptables "$@"
  fi

  # Anti-spoof: iptables -A INPUT -i <tap> ! -s <IP> -j DROP
  if [[ "$#" -eq 9 && ( "$1" == "-A" || "$1" == "-D" ) && "$2" == "INPUT" && "$3" == "-i" && "$5" == "!" && "$6" == "-s" && "$8" == "-j" && "$9" == "DROP" ]]; then
    is_tap_name "$4" || die "iptables INPUT anti-spoof: unsupported interface '$4'"
    is_ipv4 "$7" || die "iptables INPUT anti-spoof: invalid ip '$7'"
    exec /usr/sbin/iptables "$@"
  fi

  # Gateway/trusted-DNS accept: iptables -A INPUT -i <tap> -s <IP> -p tcp|udp --dport <port> -j ACCEPT
  if [[ "$#" -eq 12 && ( "$1" == "-A" || "$1" == "-D" ) && "$2" == "INPUT" && "$3" == "-i" && "$5" == "-s" && "$7" == "-p" && ( "$8" == "tcp" || "$8" == "udp" ) && "$9" == "--dport" && "${11}" == "-j" && "${12}" == "ACCEPT" ]]; then
    is_tap_name "$4" || die "iptables INPUT accept: unsupported interface '$4'"
    is_ipv4 "$6" || die "iptables INPUT accept: invalid ip '$6'"
    is_numeric "${10}" || die "iptables INPUT accept: invalid port '${10}'"
    exec /usr/sbin/iptables "$@"
  fi

  # Global gateway loopback: iptables -A|-D INPUT -i lo -p tcp --dport <port> -j ACCEPT
  if [[ "$#" -eq 10 && ( "$1" == "-A" || "$1" == "-D" ) && "$2" == "INPUT" && "$3" == "-i" && "$4" == "lo" && "$5" == "-p" && "$6" == "tcp" && "$7" == "--dport" && "$9" == "-j" && "${10}" == "ACCEPT" ]]; then
    is_numeric "$8" || die "iptables INPUT gateway loopback: invalid port '$8'"
    exec /usr/sbin/iptables "$@"
  fi

  # Global gateway drop (non-TAP): iptables -A|-D INPUT ! -i cr+ -p tcp --dport <port> -j DROP
  if [[ "$#" -eq 11 && ( "$1" == "-A" || "$1" == "-D" ) && "$2" == "INPUT" && "$3" == "!" && "$4" == "-i" && "$5" == "cr+" && "$6" == "-p" && "$7" == "tcp" && "$8" == "--dport" && "${10}" == "-j" && "${11}" == "DROP" ]]; then
    is_numeric "$9" || die "iptables INPUT gateway drop: invalid port '$9'"
    exec /usr/sbin/iptables "$@"
  fi

  # NFLOG: iptables -A|-D FORWARD -i <tap> -j NFLOG --nflog-group <group>
  if [[ "$#" -eq 8 && ( "$1" == "-A" || "$1" == "-D" ) && "$2" == "FORWARD" && "$3" == "-i" && "$5" == "-j" && "$6" == "NFLOG" && "$7" == "--nflog-group" ]]; then
    is_tap_name "$4" || die "iptables NFLOG: unsupported interface '$4'"
    is_numeric "$8" || die "iptables NFLOG: invalid group '$8'"
    exec /usr/sbin/iptables "$@"
  fi

  die "iptables: unsupported arguments"
}

run_ipset() {
  [[ "$#" -ge 1 ]] || die "ipset: missing arguments"
  local bin
  bin="$(ipset_bin)"

  if [[ "$#" -eq 7 && "$1" == "create" && "$3" == "hash:ip,port" && "$4" == "family" && "$5" == "inet" && "$6" == "timeout" ]]; then
    is_trusted_dns_set_name "$2" || die "ipset create: unsupported set '$2'"
    is_numeric "$7" || die "ipset create: invalid timeout '$7'"
    exec "$bin" "$@"
  fi

  if [[ "$#" -eq 2 && ( "$1" == "destroy" || "$1" == "flush" ) ]]; then
    is_trusted_dns_set_name "$2" || die "ipset $1: unsupported set '$2'"
    exec "$bin" "$@"
  fi

  if [[ "$#" -eq 5 && "$1" == "add" && "$4" == "timeout" ]]; then
    is_trusted_dns_set_name "$2" || die "ipset add: unsupported set '$2'"
    is_ipset_entry "$3" || die "ipset add: unsupported entry '$3'"
    is_numeric "$5" || die "ipset add: invalid timeout '$5'"
    exec "$bin" "$@"
  fi

  die "ipset: unsupported arguments"
}

run_sysctl() {
  [[ "$#" -eq 2 ]] || die "sysctl: expected 2 arguments"
  [[ "$1" == "-w" ]] || die "sysctl: unsupported flag '$1'"
  if [[ "$2" == "net.ipv4.ip_forward=1" ]]; then
    exec /usr/sbin/sysctl -w net.ipv4.ip_forward=1
  fi
  if [[ "$2" =~ ^net\.ipv6\.conf\.cr[a-z0-9]{1,13}\.disable_ipv6=1$ ]]; then
    exec /usr/sbin/sysctl -w "$2"
  fi
  die "sysctl: unsupported arguments"
}

run_zfs() {
  [[ "$#" -ge 1 ]] || die "zfs: missing arguments"
  local bin
  bin="$(zfs_bin)"

  if [[ "$#" -eq 5 && "$1" == "list" && "$2" == "-H" && "$3" == "-o" && "$4" == "name" ]]; then
    is_cleanroom_zfs_dataset "$5" || is_cleanroom_zfs_snapshot_ref "$5" || die "zfs list: unsupported ref '$5'"
    exec "$bin" "$@"
  fi

  if [[ "$#" -eq 7 && "$1" == "list" && "$2" == "-H" && "$3" == "-d" && "$4" == "0" && "$5" == "-o" && "$6" == "name" ]]; then
    is_cleanroom_zfs_dataset "$7" || die "zfs list -d 0: unsupported dataset '$7'"
    exec "$bin" "$@"
  fi

  if [[ "$#" -eq 7 && "$1" == "list" && "$2" == "-H" && "$3" == "-d" && "$4" == "1" && "$5" == "-o" && "$6" == "name" ]]; then
    is_cleanroom_zfs_snapshot_import_namespace_dataset "$7" || die "zfs list -d 1: unsupported dataset '$7'"
    exec "$bin" "$@"
  fi

  if [[ "$#" -eq 6 && "$1" == "get" && "$2" == "-H" && "$3" == "-o" && "$4" == "value" && ( "$5" == "guid" || "$5" == "origin" ) ]]; then
    is_cleanroom_zfs_dataset "$6" || is_cleanroom_zfs_snapshot_ref "$6" || die "zfs get: unsupported ref '$6'"
    exec "$bin" "$@"
  fi

  if [[ "$#" -eq 5 && "$1" == "send" && "$2" == "-nP" && "$3" == "-i" ]]; then
    is_cleanroom_zfs_snapshot_ref "$4" || die "zfs send: unsupported parent snapshot '$4'"
    is_cleanroom_zfs_stored_snapshot_ref "$5" || die "zfs send: unsupported child snapshot '$5'"
    exec "$bin" "$@"
  fi

  if [[ "$#" -eq 4 && "$1" == "send" && "$2" == "-i" ]]; then
    is_cleanroom_zfs_snapshot_ref "$3" || die "zfs send: unsupported parent snapshot '$3'"
    is_cleanroom_zfs_stored_snapshot_ref "$4" || die "zfs send: unsupported child snapshot '$4'"
    exec "$bin" "$@"
  fi

  if [[ "$#" -eq 4 && "$1" == "receive" && "$2" == "-u" && "$3" == "-F" ]]; then
    is_cleanroom_zfs_snapshot_import_dataset "$4" || die "zfs receive: unsupported dataset '$4'"
    exec "$bin" "$@"
  fi

  if [[ "$#" -eq 5 && "$1" == "create" && "$2" == "-p" && "$3" == "-V" ]]; then
    is_numeric "$4" || die "zfs create: invalid size '$4'"
    is_cleanroom_zfs_dataset "$5" || die "zfs create: unsupported dataset '$5'"
    "$bin" "$@"
    wait_for_zvol_device_path "$5"
    return
  fi

  if [[ "$#" -eq 2 && "$1" == "snapshot" ]]; then
    is_cleanroom_zfs_snapshot_ref "$2" || die "zfs snapshot: unsupported ref '$2'"
    exec "$bin" "$@"
  fi

  if [[ "$#" -eq 4 && "$1" == "clone" && "$2" == "-p" ]]; then
    is_cleanroom_zfs_snapshot_ref "$3" || die "zfs clone: unsupported snapshot '$3'"
    is_cleanroom_zfs_dataset "$4" || die "zfs clone: unsupported dataset '$4'"
    "$bin" "$@"
    wait_for_zvol_device_path "$4"
    return
  fi

  if [[ "$#" -eq 3 && "$1" == "set" && "$2" == volsize=* ]]; then
    is_numeric "${2#volsize=}" || die "zfs set: invalid volsize '${2#volsize=}'"
    is_cleanroom_zfs_dataset "$3" || die "zfs set: unsupported dataset '$3'"
    exec "$bin" "$@"
  fi

  if [[ "$#" -eq 2 && "$1" == "promote" ]]; then
    is_cleanroom_zfs_dataset "$2" || die "zfs promote: unsupported dataset '$2'"
    exec "$bin" "$@"
  fi

  if [[ "$#" -eq 3 && "$1" == "destroy" && "$2" == "-r" ]]; then
    is_cleanroom_zfs_dataset "$3" || die "zfs destroy -r: unsupported dataset '$3'"
    exec "$bin" "$@"
  fi

  if [[ "$#" -eq 2 && "$1" == "destroy" ]]; then
    is_cleanroom_zfs_snapshot_ref "$2" || die "zfs destroy: unsupported ref '$2'"
    exec "$bin" "$@"
  fi

  die "zfs: unsupported arguments"
}

run_dd() {
  [[ "$#" -eq 5 ]] || die "dd: unsupported arguments"
  [[ "$1" == if=* ]] || die "dd: missing input file"
  [[ "$2" == of=* ]] || die "dd: missing output file"
  [[ "$3" == "bs=4M" ]] || die "dd: unsupported block size"
  [[ "$4" == "conv=fsync" ]] || die "dd: unsupported conv mode"
  [[ "$5" == "status=none" ]] || die "dd: unsupported status mode"

  local src="${1#if=}"
  local dst="${2#of=}"
  is_runtime_rootfs_image "$src" || die "dd: unsupported source path '$src'"
  is_zvol_device_path "$dst" || die "dd: unsupported destination path '$dst'"

  exec "$(dd_bin)" "$@"
}

main() {
  require_root
  [[ "$#" -ge 1 ]] || die "missing command"

  local command="$1"
  shift

  case "$command" in
    version)
      [[ "$#" -eq 0 ]] || die "version: unexpected arguments"
      helper_contract_version
      ;;
    capabilities)
      [[ "$#" -eq 0 ]] || die "capabilities: unexpected arguments"
      helper_capabilities
      ;;
    true)
      [[ "$#" -eq 0 ]] || die "true: unexpected arguments"
      exec /usr/bin/true
      ;;
    ip)
      run_ip "$@"
      ;;
    ipset)
      run_ipset "$@"
      ;;
    iptables)
      run_iptables "$@"
      ;;
    sysctl)
      run_sysctl "$@"
      ;;
    zfs)
      run_zfs "$@"
      ;;
    dd)
      run_dd "$@"
      ;;
    *)
      die "unsupported command '$command'"
      ;;
  esac
}

main "$@"
