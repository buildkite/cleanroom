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

resolve_user_home() {
  local user="$1"
  local home

  home="$(dscl . -read "/Users/${user}" NFSHomeDirectory 2>/dev/null | awk '{print $2}')"
  [ -n "$home" ] || die "could not resolve home directory for user ${user}"
  printf '%s' "$home"
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

install_tailscale_if_configured() {
  local brew_bin="$1"
  local instance_id="$2"
  local tailscale_param="${TAILSCALE_AUTH_KEY_PARAM:-${CLEANROOM_TAILSCALE_AUTH_KEY_PARAMETER_NAME:-}}"
  local tailscale_hostname_prefix="${TAILSCALE_HOSTNAME_PREFIX:-${CLEANROOM_TAILSCALE_HOSTNAME_PREFIX:-}}"
  local tailscale_advertise_tags="${TAILSCALE_ADVERTISE_TAGS:-${CLEANROOM_TAILSCALE_ADVERTISE_TAGS:-}}"
  local tailscale_enable_ssh="${TAILSCALE_ENABLE_SSH:-${CLEANROOM_TAILSCALE_ENABLE_SSH:-true}}"
  local tailscale_accept_routes="${TAILSCALE_ACCEPT_ROUTES:-${CLEANROOM_TAILSCALE_ACCEPT_ROUTES:-false}}"
  local brew_dir
  local tailscale_cli
  local tailscaled_bin
  local tailscale_auth_key
  local hostname
  local -a tailscale_cmd

  if [ -z "$tailscale_param" ]; then
    return
  fi

  [ -n "$tailscale_hostname_prefix" ] || die "TAILSCALE_HOSTNAME_PREFIX must be set when Tailscale bootstrap is enabled"

  brew_dir="$(dirname "$brew_bin")"
  tailscale_cli="${brew_dir}/tailscale"
  tailscaled_bin="${brew_dir}/tailscaled"

  if [ ! -x "$tailscale_cli" ] || [ ! -x "$tailscaled_bin" ]; then
    if ! run_as_agent_user "PATH=\"${brew_dir}:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin\" \"$brew_bin\" list --formula tailscale >/dev/null 2>&1"; then
      [ "$AGENT_USER" != "root" ] || die "cannot install tailscale automatically when AGENT_USER resolves to root"
      log "installing tailscale via Homebrew"
      retry 3 5 run_as_agent_user "PATH=\"${brew_dir}:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin\" HOMEBREW_NO_AUTO_UPDATE=1 \"$brew_bin\" install --formula tailscale"
    fi
  fi

  [ -x "$tailscale_cli" ] || die "tailscale CLI missing after Homebrew install"
  [ -x "$tailscaled_bin" ] || die "tailscaled binary missing after Homebrew install"

  tailscale_auth_key="$(retry 10 3 aws ssm get-parameter --region "$AWS_REGION" --name "$tailscale_param" --with-decryption --query 'Parameter.Value' --output text)"

  if launchctl print system/com.tailscale.tailscaled >/dev/null 2>&1 || [ -f /Library/LaunchDaemons/com.tailscale.tailscaled.plist ]; then
    "$tailscaled_bin" uninstall-system-daemon || true
  fi

  "$tailscaled_bin" install-system-daemon
  launchctl enable system/com.tailscale.tailscaled
  launchctl kickstart -k system/com.tailscale.tailscaled

  hostname="${tailscale_hostname_prefix}-${instance_id}"
  tailscale_cmd=("$tailscale_cli" up --auth-key "$tailscale_auth_key" --hostname "$hostname")
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

configure_autologin_if_configured() {
  local user="$1"
  local password_param="${AUTOLOGIN_PASSWORD_PARAM:-${CLEANROOM_BUILDKITE_AUTOLOGIN_PASSWORD_PARAM:-}}"
  local password

  if [ -z "$password_param" ]; then
    log "AUTOLOGIN_PASSWORD_PARAM not set; launch agent will start after ${user} logs in"
    return
  fi

  password="$(retry 10 3 aws ssm get-parameter --region "$AWS_REGION" --name "$password_param" --with-decryption --query 'Parameter.Value' --output text)"
  [ -n "$password" ] || die "auto-login password parameter ${password_param} was empty"

  log "configuring auto-login for ${user}"
  dscl . -passwd "/Users/${user}" "$password"
  sysadminctl -autologin set -userName "$user" -password "$password" >/dev/null
  defaults write /Library/Preferences/com.apple.loginwindow autoLoginUser "$user"

  if ! sysadminctl -autologin status 2>&1 | grep -Fq "Automatic login user: ${user}"; then
    log "sysadminctl did not finish auto-login setup; writing /etc/kcpassword fallback"
    KC_PASSWORD="$password" perl -e '
      use strict;
      use warnings;

      my $password = ($ENV{KC_PASSWORD} // q{}) . "\0";
      my @key = (0x7D, 0x89, 0x52, 0x23, 0xD2, 0xBC, 0xDD, 0xEA, 0xA3, 0xB9, 0x1F);
      my @bytes = map { ord(substr($password, $_, 1)) ^ $key[$_ % scalar(@key)] } 0 .. length($password) - 1;
      while (@bytes % @key) {
        push @bytes, $key[@bytes % scalar(@key)];
      }

      open my $fh, ">:raw", "/etc/kcpassword" or die $!;
      print {$fh} pack("C*", @bytes);
      close $fh or die $!;
    '
    chown root:wheel /etc/kcpassword
    chmod 0600 /etc/kcpassword
  fi

  if ! sysadminctl -autologin status 2>&1 | grep -Fq "Automatic login user: ${user}"; then
    die "auto-login verification failed for ${user}"
  fi
}

install_pre_command_hook() {
  local hook_path="${AGENT_ROOT}/hooks/pre-command"

  if [ "$SIGNER_MODE" != "true" ]; then
    rm -f "$hook_path"
    return
  fi

  cat > "$hook_path" <<HOOK
#!/usr/bin/env bash
set -euo pipefail

require_signing_job="${SIGNER_REQUIRE_SIGNING_JOB}"
allow_tags="${SIGNER_ALLOW_TAGS}"
allow_pull_requests="${SIGNER_ALLOW_PULL_REQUESTS}"
allowed_branches="${SIGNER_ALLOWED_BRANCHES}"
allowed_branch_prefixes="${SIGNER_ALLOWED_BRANCH_PREFIXES}"

fail() {
  printf '[signer-hook] %s\n' "\$*" >&2
  exit 1
}

if [ "\${require_signing_job}" = "true" ] && [ "\${CLEANROOM_SIGNING_JOB:-}" != "1" ]; then
  fail "signer queue only accepts jobs with CLEANROOM_SIGNING_JOB=1"
fi

if [ -n "\${BUILDKITE_TAG:-}" ]; then
  if [ "\${allow_tags}" = "true" ]; then
    exit 0
  fi

  fail "tag builds are not allowed on signer queue"
fi

if [ "\${BUILDKITE_PULL_REQUEST:-false}" != "false" ] && [ "\${allow_pull_requests}" != "true" ]; then
  fail "pull request builds are not allowed on signer queue"
fi

branch="\${BUILDKITE_BRANCH:-}"
[ -n "\${branch}" ] || fail "BUILDKITE_BRANCH is empty"

allowed=false

if [ -n "\${allowed_branches}" ]; then
  IFS=',' read -r -a exact_branches <<< "\${allowed_branches}"
  for allowed_branch in "\${exact_branches[@]}"; do
    [ -n "\${allowed_branch}" ] || continue
    if [ "\${branch}" = "\${allowed_branch}" ]; then
      allowed=true
      break
    fi
  done
fi

if [ "\${allowed}" != "true" ] && [ -n "\${allowed_branch_prefixes}" ]; then
  IFS=',' read -r -a branch_prefixes <<< "\${allowed_branch_prefixes}"
  for branch_prefix in "\${branch_prefixes[@]}"; do
    [ -n "\${branch_prefix}" ] || continue
    case "\${branch}" in
      "\${branch_prefix}"*)
        allowed=true
        break
        ;;
    esac
  done
fi

if [ "\${allowed}" != "true" ]; then
  fail "branch \${branch} is not allowed on signer queue"
fi
HOOK
  chown "$AGENT_USER":"$AGENT_GROUP" "$hook_path"
  chmod 0755 "$hook_path"
  log "installed signer pre-command hook for queue ${QUEUE_NAME}"
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
AUTOLOGIN_PASSWORD_PARAM="${AUTOLOGIN_PASSWORD_PARAM:-${CLEANROOM_BUILDKITE_AUTOLOGIN_PASSWORD_PARAM:-}}"
LAUNCHAGENT_MODE="${CLEANROOM_BUILDKITE_LAUNCHAGENT_MODE:-false}"
SIGNER_MODE="${CLEANROOM_BUILDKITE_SIGNER_MODE:-false}"
SIGNER_REQUIRE_SIGNING_JOB="${CLEANROOM_SIGNER_REQUIRE_SIGNING_JOB:-true}"
SIGNER_ALLOWED_BRANCHES="${CLEANROOM_SIGNER_ALLOWED_BRANCHES:-main}"
SIGNER_ALLOWED_BRANCH_PREFIXES="${CLEANROOM_SIGNER_ALLOWED_BRANCH_PREFIXES:-}"
SIGNER_ALLOW_TAGS="${CLEANROOM_SIGNER_ALLOW_TAGS:-true}"
SIGNER_ALLOW_PULL_REQUESTS="${CLEANROOM_SIGNER_ALLOW_PULL_REQUESTS:-false}"

SIGNER_ALLOWED_BRANCHES="${SIGNER_ALLOWED_BRANCHES// /}"
SIGNER_ALLOWED_BRANCH_PREFIXES="${SIGNER_ALLOWED_BRANCH_PREFIXES// /}"

if ! id "$AGENT_USER" >/dev/null 2>&1; then
  if [ "$LAUNCHAGENT_MODE" = "true" ]; then
    die "configured agent user does not exist: ${AGENT_USER}"
  fi

  AGENT_USER="root"
fi
[ "$LAUNCHAGENT_MODE" != "true" ] || [ "$AGENT_USER" != "root" ] || die "CLEANROOM_BUILDKITE_AGENT_USER must be a real login user when launchagent mode is enabled"
AGENT_GROUP="$(id -gn "$AGENT_USER")"
AGENT_UID=""
AGENT_HOME="$AGENT_ROOT"
if [ "$AGENT_USER" != "root" ]; then
  AGENT_UID="$(id -u "$AGENT_USER")"
  AGENT_HOME="$(resolve_user_home "$AGENT_USER")"
fi

[ -n "$AWS_REGION" ] || die "AWS_REGION (or CLEANROOM_BOOTSTRAP_REGION) must be set"
[ -n "$BUILDKITE_TOKEN_PARAM" ] || die "BUILDKITE_TOKEN_PARAM must be set"

require_cmd aws
require_cmd curl
require_cmd tar
require_cmd launchctl
require_cmd plutil
if [ "$LAUNCHAGENT_MODE" = "true" ]; then
  require_cmd dscl
  require_cmd perl
  require_cmd sysadminctl
fi

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
if [ "$LAUNCHAGENT_MODE" = "true" ]; then
  install -d -o "$AGENT_USER" -g "$AGENT_GROUP" -m 0755 "${AGENT_HOME}/Library"
  install -d -o "$AGENT_USER" -g "$AGENT_GROUP" -m 0755 "${AGENT_HOME}/Library/LaunchAgents"
fi

instance_id="$(resolve_instance_id)"
install_tailscale_if_configured "$brew_bin" "$instance_id"
if [ "$LAUNCHAGENT_MODE" = "true" ]; then
  configure_autologin_if_configured "$AGENT_USER"
elif [ -n "$AUTOLOGIN_PASSWORD_PARAM" ]; then
  log "AUTOLOGIN_PASSWORD_PARAM is set but ignored because launchagent mode is disabled for queue ${QUEUE_NAME}"
fi
agent_token="$(retry 10 3 aws ssm get-parameter --region "$AWS_REGION" --name "$BUILDKITE_TOKEN_PARAM" --with-decryption --query 'Parameter.Value' --output text)"

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

install_pre_command_hook

service_label="com.buildkite.agent.${QUEUE_NAME}"
if [ "$LAUNCHAGENT_MODE" = "true" ]; then
  plist_path="${AGENT_HOME}/Library/LaunchAgents/${service_label}.plist"
  legacy_plist_path="/Library/LaunchDaemons/${service_label}.plist"
  launchd_domain="gui/${AGENT_UID}"
  launchd_target="${launchd_domain}/${service_label}"
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
  <key>WorkingDirectory</key>
  <string>${AGENT_HOME}</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>
    <string>${AGENT_HOME}</string>
    <key>PATH</key>
    <string>${AGENT_SERVICE_PATH}</string>
  </dict>
  <key>ProcessType</key>
  <string>Interactive</string>
  <key>ThrottleInterval</key>
  <integer>30</integer>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>LimitLoadToSessionType</key>
  <array>
    <string>Aqua</string>
    <string>LoginWindow</string>
    <string>Background</string>
    <string>StandardIO</string>
    <string>System</string>
  </array>
  <key>StandardOutPath</key>
  <string>${AGENT_ROOT}/logs/buildkite-agent-${QUEUE_NAME}.log</string>
  <key>StandardErrorPath</key>
  <string>${AGENT_ROOT}/logs/buildkite-agent-${QUEUE_NAME}.error.log</string>
</dict>
</plist>
PLIST
  chown "$AGENT_USER":"$AGENT_GROUP" "$plist_path"
  chmod 0644 "$plist_path"
  plutil -lint "$plist_path" >/dev/null

  rm -f "$legacy_plist_path"

  if launchctl print "system/${service_label}" >/dev/null 2>&1; then
    launchctl bootout "system/${service_label}" || true
  fi

  if launchctl print "$launchd_domain" >/dev/null 2>&1; then
    if launchctl print "$launchd_target" >/dev/null 2>&1; then
      launchctl bootout "$launchd_target" || true
    fi

    launchctl bootstrap "$launchd_domain" "$plist_path"
    launchctl enable "$launchd_target"
    launchctl kickstart -k "$launchd_target"
    log "buildkite-agent launch agent started for queue ${QUEUE_NAME} in ${launchd_domain}"
  else
    log "launch agent installed at ${plist_path}; waiting for a GUI login session for ${AGENT_USER}"
    if [ -n "$AUTOLOGIN_PASSWORD_PARAM" ]; then
      log "auto-login is configured; if the host is already booted, reboot to start the agent in an Aqua session"
    fi
  fi
else
  plist_path="/Library/LaunchDaemons/${service_label}.plist"
  launchagent_plist_path="${AGENT_HOME}/Library/LaunchAgents/${service_label}.plist"
  if [ -n "$AGENT_UID" ] && launchctl print "gui/${AGENT_UID}" >/dev/null 2>&1; then
    if launchctl print "gui/${AGENT_UID}/${service_label}" >/dev/null 2>&1; then
      launchctl bootout "gui/${AGENT_UID}/${service_label}" || true
    fi
  fi

  rm -f "$launchagent_plist_path"

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
fi
