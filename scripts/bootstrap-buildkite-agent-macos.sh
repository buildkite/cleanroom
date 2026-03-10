#!/usr/bin/env bash
set -euo pipefail

log() {
  printf '[bootstrap-buildkite-agent-macos] %s\n' "$*"
}

die() {
  printf '[bootstrap-buildkite-agent-macos] error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || die "required command not found: ${cmd}"
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

install_buildkite_agent_binary() {
  local version="$1"
  local arch
  local url
  local tmp_dir
  local binary

  arch="$(resolve_agent_arch)"
  url="https://github.com/buildkite/agent/releases/download/v${version}/buildkite-agent-darwin-${arch}-${version}.tar.gz"

  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' RETURN

  curl -fsSL "$url" -o "$tmp_dir/buildkite-agent.tgz"
  tar -xzf "$tmp_dir/buildkite-agent.tgz" -C "$tmp_dir"

  binary="$(find "$tmp_dir" -maxdepth 2 -type f -name 'buildkite-agent' | head -n 1)"
  [ -n "$binary" ] || die "buildkite-agent binary missing in release archive"

  install -o root -g wheel -m 0755 "$binary" /usr/local/bin/buildkite-agent
}

run_as_agent_user() {
  local command="$1"

  if [ "$AGENT_USER" = "root" ]; then
    bash -lc "$command"
    return
  fi

  su -l "$AGENT_USER" -c "$command"
}

resolve_brew_binary() {
  local candidate

  for candidate in /opt/homebrew/bin/brew /usr/local/bin/brew; do
    if [ -x "$candidate" ]; then
      printf '%s' "$candidate"
      return 0
    fi
  done

  if command -v brew >/dev/null 2>&1; then
    command -v brew
    return 0
  fi

  return 1
}

install_homebrew_if_missing() {
  local brew_bin

  brew_bin="$(resolve_brew_binary || true)"
  if [ -n "$brew_bin" ]; then
    printf '%s' "$brew_bin"
    return 0
  fi

  [ "$AGENT_USER" != "root" ] || die "cannot install Homebrew automatically when AGENT_USER resolves to root"

  log "installing Homebrew"
  run_as_agent_user "NONINTERACTIVE=1 /bin/bash -c \"\$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)\""

  brew_bin="$(resolve_brew_binary || true)"
  [ -n "$brew_bin" ] || die "Homebrew installation completed but brew binary was not found"
  printf '%s' "$brew_bin"
}

has_e2fsprogs() {
  local root

  for root in /opt/homebrew/opt/e2fsprogs /usr/local/opt/e2fsprogs; do
    if [ -x "$root/sbin/mkfs.ext4" ] && [ -x "$root/sbin/debugfs" ]; then
      return 0
    fi
  done

  return 1
}

install_e2fsprogs_if_missing() {
  local brew_bin="$1"
  local brew_dir

  if has_e2fsprogs; then
    return
  fi

  [ "$AGENT_USER" != "root" ] || die "cannot install e2fsprogs automatically when AGENT_USER resolves to root"

  brew_dir="$(dirname "$brew_bin")"
  log "installing e2fsprogs via Homebrew"
  run_as_agent_user "PATH=\"${brew_dir}:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin\" HOMEBREW_NO_AUTO_UPDATE=1 \"$brew_bin\" install e2fsprogs"

  has_e2fsprogs || die "e2fsprogs installation completed but mkfs.ext4/debugfs were not found"
}

resolve_instance_id() {
  if [ -n "${CLEANROOM_BOOTSTRAP_INSTANCE_ID:-}" ]; then
    printf '%s' "$CLEANROOM_BOOTSTRAP_INSTANCE_ID"
    return
  fi

  local imds_token
  local instance_id

  imds_token="$(curl -fsS -m 2 -X PUT 'http://169.254.169.254/latest/api/token' -H 'X-aws-ec2-metadata-token-ttl-seconds: 21600' || true)"
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
NAME_PREFIX="${CLEANROOM_BOOTSTRAP_NAME_PREFIX:-cleanroom-ci-mac}"
QUEUE_NAME="${CLEANROOM_BUILDKITE_QUEUE:-cleanroom-mac}"
AGENT_VERSION="${CLEANROOM_BUILDKITE_AGENT_VERSION:-3.119.1}"
AGENT_ROOT="/var/lib/buildkite-agent"
BUILD_PATH="${CLEANROOM_BUILDKITE_BUILD_PATH:-${AGENT_ROOT}/builds}"
CONFIG_DIR="/usr/local/etc/buildkite-agent"
AGENT_USER="${CLEANROOM_BUILDKITE_AGENT_USER:-ec2-user}"
AGENT_SERVICE_PATH="/opt/homebrew/opt/e2fsprogs/sbin:/opt/homebrew/opt/e2fsprogs/bin:/usr/local/opt/e2fsprogs/sbin:/usr/local/opt/e2fsprogs/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

if ! id "$AGENT_USER" >/dev/null 2>&1; then
  AGENT_USER="root"
fi
AGENT_GROUP="$(id -gn "$AGENT_USER")"

[ -n "$AWS_REGION" ] || die "AWS_REGION (or CLEANROOM_BOOTSTRAP_REGION) must be set"
[ -n "$BUILDKITE_TOKEN_PARAM" ] || die "BUILDKITE_TOKEN_PARAM must be set"

require_cmd aws
require_cmd curl
require_cmd tar
require_cmd launchctl
require_cmd plutil

brew_bin="$(install_homebrew_if_missing)"
install_e2fsprogs_if_missing "$brew_bin"

install -d -o root -g wheel -m 0755 /usr/local/bin
log "installing buildkite-agent ${AGENT_VERSION}"
install_buildkite_agent_binary "$AGENT_VERSION"

install -d -o root -g wheel -m 0755 "$CONFIG_DIR"
install -d -o "$AGENT_USER" -g "$AGENT_GROUP" -m 0755 "$AGENT_ROOT"
install -d -o "$AGENT_USER" -g "$AGENT_GROUP" -m 0755 "$AGENT_ROOT/logs"
install -d -o "$AGENT_USER" -g "$AGENT_GROUP" -m 0755 "$BUILD_PATH"
install -d -o "$AGENT_USER" -g "$AGENT_GROUP" -m 0755 "$AGENT_ROOT/hooks"
install -d -o "$AGENT_USER" -g "$AGENT_GROUP" -m 0755 "$AGENT_ROOT/plugins"

agent_token="$(aws ssm get-parameter --region "$AWS_REGION" --name "$BUILDKITE_TOKEN_PARAM" --with-decryption --query 'Parameter.Value' --output text)"
instance_id="$(resolve_instance_id)"

tags="queue=${QUEUE_NAME},host=${instance_id},os=macos,role=cleanroom,name_prefix=${NAME_PREFIX}"
config_path="${CONFIG_DIR}/${QUEUE_NAME}.cfg"
cat > "$config_path" <<CFG
token="$agent_token"
name="${NAME_PREFIX}-${instance_id}-${QUEUE_NAME}"
tags="$tags"
build-path="${BUILD_PATH}"
hooks-path="${AGENT_ROOT}/hooks"
plugins-path="${AGENT_ROOT}/plugins"
CFG
chown root:"$AGENT_GROUP" "$config_path"
chmod 0640 "$config_path"

service_label="com.buildkite.agent.${QUEUE_NAME}"
plist_path="/Library/LaunchDaemons/${service_label}.plist"
cat > "$plist_path" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${service_label}</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/buildkite-agent</string>
    <string>start</string>
    <string>--config</string>
    <string>${config_path}</string>
  </array>
  <key>UserName</key>
  <string>${AGENT_USER}</string>
  <key>WorkingDirectory</key>
  <string>${AGENT_ROOT}</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>
    <string>${AGENT_ROOT}</string>
    <key>PATH</key>
    <string>${AGENT_SERVICE_PATH}</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>${AGENT_ROOT}/logs/buildkite-agent-${QUEUE_NAME}.log</string>
  <key>StandardErrorPath</key>
  <string>${AGENT_ROOT}/logs/buildkite-agent-${QUEUE_NAME}.error.log</string>
</dict>
</plist>
PLIST
chmod 0644 "$plist_path"
plutil -lint "$plist_path" >/dev/null

if launchctl print "system/${service_label}" >/dev/null 2>&1; then
  launchctl bootout system "$plist_path" || true
fi

launchctl bootstrap system "$plist_path"
launchctl enable "system/${service_label}"
launchctl kickstart -k "system/${service_label}"

log "buildkite-agent service started for queue ${QUEUE_NAME}"
