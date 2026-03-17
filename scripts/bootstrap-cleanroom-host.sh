#!/usr/bin/env bash
set -euo pipefail

log() {
  printf '[bootstrap-cleanroom-host] %s\n' "$*"
}

warn() {
  printf '[bootstrap-cleanroom-host] warning: %s\n' "$*" >&2
}

die() {
  printf '[bootstrap-cleanroom-host] error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || die "required command not found: ${cmd}"
}

retry() {
  local attempts="$1"
  local delay_seconds="$2"
  shift 2

  local n=1
  while true; do
    if "$@"; then
      return 0
    fi

    if [ "$n" -ge "$attempts" ]; then
      return 1
    fi

    sleep "$delay_seconds"
    n=$((n + 1))
  done
}

configure_apt_ipv4() {
  install -d -o root -g root -m 0755 /etc/apt/apt.conf.d
  cat > /etc/apt/apt.conf.d/99force-ipv4 <<'APTCONF'
Acquire::ForceIPv4 "true";
APTCONF
}

install_sudo_if_missing() {
  if command -v sudo >/dev/null 2>&1; then
    return
  fi

  configure_apt_ipv4
  export DEBIAN_FRONTEND=noninteractive
  retry 5 10 apt-get update -y
  retry 5 10 apt-get install -y sudo
}

resolve_instance_id() {
  if [ -n "${CLEANROOM_BOOTSTRAP_INSTANCE_ID:-}" ]; then
    printf '%s' "$CLEANROOM_BOOTSTRAP_INSTANCE_ID"
    return
  fi

  local imds_token
  local instance_id
  imds_token="$(curl -fsS -m 2 -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 21600" || true)"
  if [ -z "$imds_token" ]; then
    printf 'unknown-instance'
    return
  fi

  instance_id="$(curl -fsS -m 2 -H "X-aws-ec2-metadata-token: $imds_token" http://169.254.169.254/latest/meta-data/instance-id || true)"
  if [ -z "$instance_id" ]; then
    printf 'unknown-instance'
    return
  fi

  printf '%s' "$instance_id"
}

install_tailscale_if_configured() {
  local tailscale_param="${TAILSCALE_AUTH_KEY_PARAMETER_NAME:-${CLEANROOM_TAILSCALE_AUTH_KEY_PARAMETER_NAME:-}}"
  local tailscale_version="${TAILSCALE_VERSION:-${CLEANROOM_TAILSCALE_VERSION:-}}"
  local tailscale_hostname_prefix="${TAILSCALE_HOSTNAME_PREFIX:-${CLEANROOM_TAILSCALE_HOSTNAME_PREFIX:-}}"
  local tailscale_advertise_tags="${TAILSCALE_ADVERTISE_TAGS:-${CLEANROOM_TAILSCALE_ADVERTISE_TAGS:-}}"
  local tailscale_enable_ssh="${TAILSCALE_ENABLE_SSH:-${CLEANROOM_TAILSCALE_ENABLE_SSH:-true}}"
  local tailscale_accept_routes="${TAILSCALE_ACCEPT_ROUTES:-${CLEANROOM_TAILSCALE_ACCEPT_ROUTES:-false}}"
  local tailscale_auth_key
  local arch_name
  local ts_arch='amd64'
  local ts_url
  local ts_tmp_dir
  local ts_extract_dir
  local instance_id
  local -a tailscale_cmd

  if [ -z "$tailscale_param" ]; then
    return
  fi

  if ! command -v aws >/dev/null 2>&1; then
    warn "skipping tailscale bootstrap because aws CLI is not installed"
    return
  fi

  [ -n "$AWS_REGION" ] || die "AWS_REGION (or CLEANROOM_BOOTSTRAP_REGION) must be set when tailscale bootstrap is enabled"
  [ -n "$tailscale_version" ] || die "TAILSCALE_VERSION (or CLEANROOM_TAILSCALE_VERSION) must be set when tailscale bootstrap is enabled"
  [ -n "$tailscale_hostname_prefix" ] || die "TAILSCALE_HOSTNAME_PREFIX (or CLEANROOM_TAILSCALE_HOSTNAME_PREFIX) must be set when tailscale bootstrap is enabled"

  log "installing tailscale ${tailscale_version}"
  tailscale_auth_key="$(retry 10 3 aws ssm get-parameter --region "$AWS_REGION" --name "$tailscale_param" --with-decryption --query 'Parameter.Value' --output text)"

  arch_name="$(uname -m)"
  if [ "$arch_name" = 'aarch64' ] || [ "$arch_name" = 'arm64' ]; then
    ts_arch='arm64'
  fi

  ts_url="$(printf 'https://pkgs.tailscale.com/stable/tailscale_%s_%s.tgz' "$tailscale_version" "$ts_arch")"
  ts_tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$ts_tmp_dir"' RETURN

  retry 5 5 curl -fsSL "$ts_url" -o "$ts_tmp_dir/tailscale.tgz"
  tar -xzf "$ts_tmp_dir/tailscale.tgz" -C "$ts_tmp_dir"
  ts_extract_dir="$(find "$ts_tmp_dir" -maxdepth 1 -type d -name 'tailscale_*' | head -n 1)"
  [ -n "$ts_extract_dir" ] || die "tailscale archive missing extracted directory"

  install -o root -g root -m 0755 "$ts_extract_dir/tailscale" /usr/local/bin/tailscale
  install -o root -g root -m 0755 "$ts_extract_dir/tailscaled" /usr/local/bin/tailscaled
  install -d -o root -g root -m 0755 /var/lib/tailscale

  cat > /etc/systemd/system/tailscaled.service <<'TAILSCALE_UNIT'
[Unit]
Description=Tailscale node agent
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
RuntimeDirectory=tailscale
ExecStart=/usr/local/bin/tailscaled --state=/var/lib/tailscale/tailscaled.state --socket=/run/tailscale/tailscaled.sock
Restart=on-failure

[Install]
WantedBy=multi-user.target
TAILSCALE_UNIT

  systemctl daemon-reload
  systemctl enable tailscaled
  if systemctl is-active --quiet tailscaled; then
    systemctl restart tailscaled
  else
    systemctl start tailscaled
  fi

  instance_id="$(resolve_instance_id)"
  tailscale_cmd=(/usr/local/bin/tailscale up --auth-key "$tailscale_auth_key" --hostname "${tailscale_hostname_prefix}-${instance_id}")
  if [ "$tailscale_enable_ssh" = "true" ]; then
    tailscale_cmd+=(--ssh)
  fi
  if [ "$tailscale_accept_routes" = "true" ]; then
    tailscale_cmd+=(--accept-routes)
  fi
  if [ -n "$tailscale_advertise_tags" ]; then
    tailscale_cmd+=(--advertise-tags "$tailscale_advertise_tags")
  fi

  retry 5 5 "${tailscale_cmd[@]}"
}

resolve_firecracker_arch() {
  case "$(uname -m)" in
    x86_64|amd64)
      printf 'x86_64'
      ;;
    arm64|aarch64)
      printf 'aarch64'
      ;;
    *)
      die "unsupported architecture for firecracker: $(uname -m)"
      ;;
  esac
}

install_firecracker_binary() {
  local version="$1"
  local arch
  local url
  local tmp_dir
  local binary

  arch="$(resolve_firecracker_arch)"
  url="https://github.com/firecracker-microvm/firecracker/releases/download/v${version}/firecracker-v${version}-${arch}.tgz"

  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' RETURN

  retry 5 5 curl -fsSL "$url" -o "$tmp_dir/firecracker.tgz"
  tar -xzf "$tmp_dir/firecracker.tgz" -C "$tmp_dir"

  binary="$(find "$tmp_dir" -type f -name "firecracker-v${version}-${arch}" | head -n 1)"
  [ -n "$binary" ] || die "firecracker binary missing in release archive"

  install -o root -g root -m 0755 "$binary" /usr/local/bin/firecracker
}

if [ "$(id -u)" -ne 0 ]; then
  die "must run as root"
fi

NAME_PREFIX="${CLEANROOM_BOOTSTRAP_NAME_PREFIX:-cleanroom-prod}"
INSTALL_FIRECRACKER="${CLEANROOM_INSTALL_FIRECRACKER:-true}"
FIRECRACKER_VERSION="${CLEANROOM_FIRECRACKER_VERSION:-1.14.2}"
CLEANROOM_BINARY_INSTALL_DIR="${CLEANROOM_BINARY_INSTALL_DIR:-/usr/local/bin}"
CLEANROOM_CONFIG_DIR="${CLEANROOM_CONFIG_DIR:-/root/.config/cleanroom}"
CLEANROOM_FIRECRACKER_VCPUS="${CLEANROOM_FIRECRACKER_VCPUS:-4}"
CLEANROOM_FIRECRACKER_MEMORY_MIB="${CLEANROOM_FIRECRACKER_MEMORY_MIB:-8192}"
CLEANROOM_FIRECRACKER_LAUNCH_SECONDS="${CLEANROOM_FIRECRACKER_LAUNCH_SECONDS:-90}"
AWS_REGION="${AWS_REGION:-${CLEANROOM_BOOTSTRAP_REGION:-}}"
TAILSCALE_AUTH_KEY_PARAMETER_NAME="${TAILSCALE_AUTH_KEY_PARAMETER_NAME:-${CLEANROOM_TAILSCALE_AUTH_KEY_PARAMETER_NAME:-}}"
TAILSCALE_VERSION="${TAILSCALE_VERSION:-${CLEANROOM_TAILSCALE_VERSION:-1.82.5}}"
TAILSCALE_HOSTNAME_PREFIX="${TAILSCALE_HOSTNAME_PREFIX:-${CLEANROOM_TAILSCALE_HOSTNAME_PREFIX:-}}"
TAILSCALE_ADVERTISE_TAGS="${TAILSCALE_ADVERTISE_TAGS:-${CLEANROOM_TAILSCALE_ADVERTISE_TAGS:-}}"
TAILSCALE_ENABLE_SSH="${TAILSCALE_ENABLE_SSH:-${CLEANROOM_TAILSCALE_ENABLE_SSH:-true}}"
TAILSCALE_ACCEPT_ROUTES="${TAILSCALE_ACCEPT_ROUTES:-${CLEANROOM_TAILSCALE_ACCEPT_ROUTES:-false}}"

HOME="${HOME:-/root}"
export HOME
export XDG_CONFIG_HOME="${XDG_CONFIG_HOME:-$HOME/.config}"
export XDG_STATE_HOME="${XDG_STATE_HOME:-$HOME/.local/state}"
export XDG_DATA_HOME="${XDG_DATA_HOME:-$HOME/.local/share}"
export GOPATH="${GOPATH:-$HOME/go}"
export GOMODCACHE="${GOMODCACHE:-$GOPATH/pkg/mod}"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"

require_cmd apt-get
require_cmd curl
require_cmd tar
require_cmd systemctl

install_sudo_if_missing
install_tailscale_if_configured

configure_apt_ipv4
export DEBIAN_FRONTEND=noninteractive
retry 5 10 apt-get update -y
retry 5 10 apt-get install -y e2fsprogs golang-go iproute2 iptables

if [ "$INSTALL_FIRECRACKER" = "true" ]; then
  log "installing firecracker ${FIRECRACKER_VERSION}"
  install_firecracker_binary "$FIRECRACKER_VERSION"
fi

install -d -o root -g root -m 0755 "$XDG_CONFIG_HOME" "$XDG_STATE_HOME" "$XDG_DATA_HOME"
install -d -o root -g root -m 0755 "$GOPATH" "$GOPATH/pkg" "$GOMODCACHE"

log "building cleanroom binaries from ${REPO_ROOT}"
export GOTOOLCHAIN=auto
(
  cd "$REPO_ROOT"
  scripts/build-go.sh
)

install -d -o root -g root -m 0755 "$CLEANROOM_BINARY_INSTALL_DIR"
install -o root -g root -m 0755 "$REPO_ROOT/dist/cleanroom" "$CLEANROOM_BINARY_INSTALL_DIR/cleanroom"
install -o root -g root -m 0755 "$REPO_ROOT/dist/cleanroom-guest-agent" "$CLEANROOM_BINARY_INSTALL_DIR/cleanroom-guest-agent"

snapshot_base_dir='/var/lib/cleanroom/snapshots'
CLEANROOM_ZFS_DATASET="${CLEANROOM_ZFS_DATASET:-cleanroom/data}"
if command -v zpool >/dev/null 2>&1 && zpool list cleanroom >/dev/null 2>&1; then
  if command -v zfs >/dev/null 2>&1; then
    dataset_mountpoint="$(zfs get -H -o value mountpoint "$CLEANROOM_ZFS_DATASET" 2>/dev/null || true)"
    case "$dataset_mountpoint" in
      ""|none|legacy|-)
        ;;
      *)
        snapshot_base_dir="$dataset_mountpoint/snapshots"
        ;;
    esac
  fi
fi

install -d -o root -g root -m 0755 "$snapshot_base_dir"
install -d -o root -g root -m 0755 "$CLEANROOM_CONFIG_DIR"

cat > "$CLEANROOM_CONFIG_DIR/config.yaml" <<EOF
default_backend: firecracker
backends:
  firecracker:
    binary_path: /usr/local/bin/firecracker
    vcpus: ${CLEANROOM_FIRECRACKER_VCPUS}
    memory_mib: ${CLEANROOM_FIRECRACKER_MEMORY_MIB}
    launch_seconds: ${CLEANROOM_FIRECRACKER_LAUNCH_SECONDS}
    snapshots:
      enabled: true
      driver: file
      base_dir: ${snapshot_base_dir}
      quiesce_timeout_seconds: 15
EOF

chmod 0644 "$CLEANROOM_CONFIG_DIR/config.yaml"

log "running cleanroom doctor"
"$CLEANROOM_BINARY_INSTALL_DIR/cleanroom" doctor

log "installing cleanroom system daemon"
"$CLEANROOM_BINARY_INSTALL_DIR/cleanroom" daemon install --force --log-level info

systemctl is-active --quiet cleanroom.service || die "cleanroom.service failed to start"

log "cleanroom host ready (${NAME_PREFIX})"
