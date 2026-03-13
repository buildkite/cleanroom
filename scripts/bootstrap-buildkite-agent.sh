#!/usr/bin/env bash
set -euo pipefail

log() {
  printf '[bootstrap-buildkite-agent] %s\n' "$*"
}

warn() {
  printf '[bootstrap-buildkite-agent] warning: %s\n' "$*" >&2
}

die() {
  printf '[bootstrap-buildkite-agent] error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || die "required command not found: ${cmd}"
}

install_sudo_if_missing() {
  if command -v sudo >/dev/null 2>&1; then
    return
  fi

  if command -v dnf >/dev/null 2>&1; then
    dnf install -y sudo
    return
  fi

  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -y
    apt-get install -y sudo
    return
  fi

  if command -v yum >/dev/null 2>&1; then
    yum install -y sudo
    return
  fi

  die "sudo is required and no supported package manager was found"
}

resolve_agent_arch() {
  case "$(uname -m)" in
    x86_64|amd64)
      printf 'amd64'
      ;;
    arm64|aarch64)
      printf 'arm64'
      ;;
    *)
      die "unsupported architecture for buildkite-agent: $(uname -m)"
      ;;
  esac
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

install_buildkite_agent_binary() {
  local version="$1"
  local arch
  local url
  local tmp_dir
  local binary

  arch="$(resolve_agent_arch)"
  url="https://github.com/buildkite/agent/releases/download/v${version}/buildkite-agent-linux-${arch}-${version}.tar.gz"

  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' RETURN

  curl -fsSL "$url" -o "$tmp_dir/buildkite-agent.tgz"
  tar -xzf "$tmp_dir/buildkite-agent.tgz" -C "$tmp_dir"

  binary="$(find "$tmp_dir" -maxdepth 2 -type f -name 'buildkite-agent' | head -n 1)"
  [ -n "$binary" ] || die "buildkite-agent binary missing in release archive"

  install -o root -g root -m 0755 "$binary" /usr/local/bin/buildkite-agent
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

  curl -fsSL "$url" -o "$tmp_dir/firecracker.tgz"
  tar -xzf "$tmp_dir/firecracker.tgz" -C "$tmp_dir"

  binary="$(find "$tmp_dir" -type f -name "firecracker-v${version}-${arch}" | head -n 1)"
  [ -n "$binary" ] || die "firecracker binary missing in release archive"

  install -o root -g root -m 0755 "$binary" /usr/local/bin/firecracker
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

if [ "$(id -u)" -ne 0 ]; then
  die "must run as root"
fi

AWS_REGION="${AWS_REGION:-${CLEANROOM_BOOTSTRAP_REGION:-}}"
BUILDKITE_TOKEN_PARAM="${BUILDKITE_TOKEN_PARAM:-}"
NAME_PREFIX="${CLEANROOM_BOOTSTRAP_NAME_PREFIX:-cleanroom-ci}"
QUEUE_NAME="${CLEANROOM_BUILDKITE_QUEUE:-cleanroom}"
AGENT_VERSION="${CLEANROOM_BUILDKITE_AGENT_VERSION:-3.119.1}"
INSTALL_FIRECRACKER="${CLEANROOM_INSTALL_FIRECRACKER:-true}"
FIRECRACKER_VERSION="${CLEANROOM_FIRECRACKER_VERSION:-1.14.2}"
KERNEL_IMAGE_URL="${CLEANROOM_KERNEL_IMAGE_URL:-https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/kernels/vmlinux.bin}"
HELPER_INSTALL_PATH="${CLEANROOM_HELPER_INSTALL_PATH:-/usr/local/sbin/cleanroom-root-helper}"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
HELPER_SOURCE_PATH="${CLEANROOM_HELPER_SOURCE_PATH:-$REPO_ROOT/scripts/cleanroom-root-helper.sh}"

[ -n "$AWS_REGION" ] || die "AWS_REGION (or CLEANROOM_BOOTSTRAP_REGION) must be set"
[ -n "$BUILDKITE_TOKEN_PARAM" ] || die "BUILDKITE_TOKEN_PARAM must be set"

require_cmd aws
require_cmd curl
require_cmd tar
require_cmd systemctl
install_sudo_if_missing

log "installing buildkite-agent ${AGENT_VERSION}"
install_buildkite_agent_binary "$AGENT_VERSION"

if ! id buildkite-agent >/dev/null 2>&1; then
  useradd --system --create-home --home-dir /var/lib/buildkite-agent --shell /bin/bash buildkite-agent
fi

install -d -o buildkite-agent -g buildkite-agent -m 0700 /var/lib/buildkite-agent/.ssh
install -d -o buildkite-agent -g buildkite-agent -m 0755 /var/lib/buildkite-agent/builds
install -d -o buildkite-agent -g buildkite-agent -m 0755 /var/lib/buildkite-agent/hooks
install -d -o buildkite-agent -g buildkite-agent -m 0755 /var/lib/buildkite-agent/plugins
install -d -o buildkite-agent -g buildkite-agent -m 0755 /var/lib/buildkite-agent/.local
install -d -o buildkite-agent -g buildkite-agent -m 0755 /var/lib/buildkite-agent/.local/share
install -d -o buildkite-agent -g buildkite-agent -m 0755 /var/lib/buildkite-agent/.local/share/cleanroom
install -d -o buildkite-agent -g buildkite-agent -m 0755 /var/lib/buildkite-agent/.local/share/cleanroom/images
install -d -o root -g root -m 0755 /etc/buildkite-agent

agent_token="$(aws ssm get-parameter --region "$AWS_REGION" --name "$BUILDKITE_TOKEN_PARAM" --with-decryption --query 'Parameter.Value' --output text)"
instance_id="$(resolve_instance_id)"

cat > /etc/systemd/system/buildkite-agent@.service <<'UNIT'
[Unit]
Description=Buildkite Agent (%i)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=buildkite-agent
Group=buildkite-agent
Environment=HOME=/var/lib/buildkite-agent
ExecStart=/usr/local/bin/buildkite-agent start --config /etc/buildkite-agent/%i.cfg
Restart=always
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
UNIT

tags="queue=${QUEUE_NAME},host=${instance_id},os=linux,role=cleanroom,name_prefix=${NAME_PREFIX}"
cat > "/etc/buildkite-agent/${QUEUE_NAME}.cfg" <<CFG
token="$agent_token"
name="${NAME_PREFIX}-${instance_id}-${QUEUE_NAME}"
tags="$tags"
build-path="/var/lib/buildkite-agent/builds"
hooks-path="/var/lib/buildkite-agent/hooks"
plugins-path="/var/lib/buildkite-agent/plugins"
environment=["CLEANROOM_PRIVILEGED_MODE=helper","CLEANROOM_PRIVILEGED_HELPER_PATH=${HELPER_INSTALL_PATH}"]
CFG
chown root:buildkite-agent "/etc/buildkite-agent/${QUEUE_NAME}.cfg"
chmod 0640 "/etc/buildkite-agent/${QUEUE_NAME}.cfg"

if [ ! -f "$HELPER_SOURCE_PATH" ]; then
  die "cleanroom helper script not found at ${HELPER_SOURCE_PATH}"
fi
install -o root -g root -m 0755 "$HELPER_SOURCE_PATH" "$HELPER_INSTALL_PATH"

printf 'buildkite-agent ALL=(root) NOPASSWD: %s *\n' "$HELPER_INSTALL_PATH" > /etc/sudoers.d/buildkite-cleanroom
chmod 0440 /etc/sudoers.d/buildkite-cleanroom

cat > /var/lib/buildkite-agent/hooks/pre-command <<HOOK
#!/usr/bin/env bash
set -euo pipefail

[ -x "$HELPER_INSTALL_PATH" ] || {
  echo "cleanroom-root-helper missing at $HELPER_INSTALL_PATH" >&2
  exit 1
}
HOOK
chown buildkite-agent:buildkite-agent /var/lib/buildkite-agent/hooks/pre-command
chmod 0755 /var/lib/buildkite-agent/hooks/pre-command

if [ "$INSTALL_FIRECRACKER" = "true" ]; then
  log "installing firecracker ${FIRECRACKER_VERSION}"
  install_firecracker_binary "$FIRECRACKER_VERSION"
  curl -fsSL "$KERNEL_IMAGE_URL" -o /var/lib/buildkite-agent/.local/share/cleanroom/images/vmlinux.bin
  chown buildkite-agent:buildkite-agent /var/lib/buildkite-agent/.local/share/cleanroom/images/vmlinux.bin
  chmod 0644 /var/lib/buildkite-agent/.local/share/cleanroom/images/vmlinux.bin
fi

if getent group kvm >/dev/null 2>&1; then
  usermod -aG kvm buildkite-agent
fi

# Configure cleanroom runtime config with ZFS snapshot support if a pool exists.
CLEANROOM_ZFS_DATASET="${CLEANROOM_ZFS_DATASET:-cleanroom/data}"
if command -v zpool >/dev/null 2>&1 && zpool list cleanroom >/dev/null 2>&1; then
  log "configuring cleanroom runtime config with ZFS snapshot support (dataset: ${CLEANROOM_ZFS_DATASET})"
  agent_config_dir="/var/lib/buildkite-agent/.config/cleanroom"
  install -d -o buildkite-agent -g buildkite-agent -m 0755 "$agent_config_dir"
  cat > "$agent_config_dir/config.yaml" <<RTCFG
backends:
  firecracker:
    snapshots:
      enabled: true
      driver: zfs
      zfs_dataset: ${CLEANROOM_ZFS_DATASET}
      quiesce_timeout_seconds: 15
RTCFG
  chown buildkite-agent:buildkite-agent "$agent_config_dir/config.yaml"
  chmod 0644 "$agent_config_dir/config.yaml"
fi

systemctl daemon-reload
systemctl enable --now "buildkite-agent@${QUEUE_NAME}.service"

log "buildkite-agent service started for queue ${QUEUE_NAME}"
