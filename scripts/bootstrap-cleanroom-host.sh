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

normalize_version() {
  local raw="$1"
  if [ -z "$raw" ] || [ "$raw" = "latest" ]; then
    printf 'latest'
    return
  fi

  case "$raw" in
    v*) printf '%s' "$raw" ;;
    *) printf 'v%s' "$raw" ;;
  esac
}

github_repo_slug_from_url() {
  local raw="$1"
  raw="${raw%.git}"

  case "$raw" in
    git@github.com:*)
      printf '%s' "${raw#git@github.com:}"
      ;;
    https://github.com/*)
      printf '%s' "${raw#https://github.com/}"
      ;;
    ssh://git@github.com/*)
      printf '%s' "${raw#ssh://git@github.com/}"
      ;;
    git://github.com/*)
      printf '%s' "${raw#git://github.com/}"
      ;;
    *)
      return 1
      ;;
  esac
}

raw_github_url() {
  local repo="$1"
  local ref="$2"
  local path="$3"
  printf 'https://raw.githubusercontent.com/%s/%s/%s' "$repo" "$ref" "$path"
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

install_host_bootstrap_runner() {
  local bootstrap_env_path='/usr/local/etc/cleanroom-bootstrap-host.env'
  local bootstrap_runner_path='/usr/local/bin/cleanroom-bootstrap-host'
  local bootstrap_repo_url="${CLEANROOM_BOOTSTRAP_REPO_URL:-}"
  local bootstrap_repo_ref="${CLEANROOM_BOOTSTRAP_REPO_REF:-main}"
  local bootstrap_setup_script_path="${CLEANROOM_BOOTSTRAP_SETUP_SCRIPT_PATH:-scripts/bootstrap-cleanroom-host.sh}"
  local bootstrap_deploy_key_param="${CLEANROOM_BOOTSTRAP_DEPLOY_KEY_PARAM:-}"
  local bootstrap_tailscale_param="${TAILSCALE_AUTH_KEY_PARAMETER_NAME:-${CLEANROOM_TAILSCALE_AUTH_KEY_PARAMETER_NAME:-}}"
  local bootstrap_zfs_dataset="${CLEANROOM_ZFS_DATASET:-cleanroom/data}"

  if [ -z "$bootstrap_repo_url" ]; then
    warn "skipping host bootstrap runner install because CLEANROOM_BOOTSTRAP_REPO_URL is not set"
    return
  fi

  install -d -o root -g root -m 0755 /usr/local/etc
  install -d -o root -g root -m 0755 /usr/local/bin

  cat > "$bootstrap_env_path" <<EOF
AWS_REGION='$AWS_REGION'
NAME_PREFIX='$NAME_PREFIX'
DEPLOY_KEY_PARAM='$bootstrap_deploy_key_param'
REPO_URL='$bootstrap_repo_url'
REPO_REF='$bootstrap_repo_ref'
SETUP_SCRIPT_PATH='$bootstrap_setup_script_path'
INSTALL_FIRECRACKER='$INSTALL_FIRECRACKER'
FIRECRACKER_VERSION='$FIRECRACKER_VERSION'
CLEANROOM_VERSION='$CLEANROOM_VERSION'
CLEANROOM_INSTALL_SCRIPT_REF='$CLEANROOM_INSTALL_SCRIPT_REF'
CLEANROOM_BINARY_INSTALL_DIR='$CLEANROOM_BINARY_INSTALL_DIR'
CLEANROOM_CONFIG_DIR='$CLEANROOM_CONFIG_DIR'
HELPER_INSTALL_PATH='$HELPER_INSTALL_PATH'
TAILSCALE_AUTH_KEY_PARAMETER_NAME='$bootstrap_tailscale_param'
TAILSCALE_VERSION='$TAILSCALE_VERSION'
TAILSCALE_HOSTNAME_PREFIX='$TAILSCALE_HOSTNAME_PREFIX'
TAILSCALE_ADVERTISE_TAGS='$TAILSCALE_ADVERTISE_TAGS'
TAILSCALE_ENABLE_SSH='$TAILSCALE_ENABLE_SSH'
TAILSCALE_ACCEPT_ROUTES='$TAILSCALE_ACCEPT_ROUTES'
ZFS_DATASET='$bootstrap_zfs_dataset'
EOF
  chmod 0600 "$bootstrap_env_path"

  cat > "$bootstrap_runner_path" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

BOOTSTRAP_ENV_PATH='/usr/local/etc/cleanroom-bootstrap-host.env'
BOOTSTRAP_RUNNER_PATH='/usr/local/bin/cleanroom-bootstrap-host'
exec > >(tee -a /var/log/cleanroom-bootstrap-host.log) 2>&1

if [ ! -f "$BOOTSTRAP_ENV_PATH" ]; then
  echo "bootstrap env file missing: $BOOTSTRAP_ENV_PATH" >&2
  exit 1
fi

# shellcheck disable=SC1091
source "$BOOTSTRAP_ENV_PATH"
PATH="/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"

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

read_tfvars_string() {
  local path="$1"
  local key="$2"

  awk -v key="$key" '
    $0 ~ "^[[:space:]]*" key "[[:space:]]*=" {
      if (match($0, /^[[:space:]]*[A-Za-z0-9_]+[[:space:]]*=[[:space:]]*"([^"]*)"/, m)) {
        print m[1]
        found = 1
      }
      exit
    }
    END { exit(found ? 0 : 1) }
  ' "$path"
}

read_tfvars_bool() {
  local path="$1"
  local key="$2"

  awk -v key="$key" '
    $0 ~ "^[[:space:]]*" key "[[:space:]]*=" {
      if (match($0, /^[[:space:]]*[A-Za-z0-9_]+[[:space:]]*=[[:space:]]*(true|false)/, m)) {
        print m[1]
        found = 1
      }
      exit
    }
    END { exit(found ? 0 : 1) }
  ' "$path"
}

apply_prod_tfvars_overrides() {
  local repo_root="$1"
  local tfvars_path="${BOOTSTRAP_TERRAFORM_VAR_FILE:-}"
  local value

  if [ -z "$tfvars_path" ]; then
    tfvars_path="$repo_root/infra/terraform/envs/prod/prod.${AWS_REGION}.tfvars"
  elif [ "${tfvars_path#/}" = "$tfvars_path" ]; then
    tfvars_path="$repo_root/$tfvars_path"
  fi

  if [ ! -f "$tfvars_path" ]; then
    echo "warning: bootstrap tfvars not found: $tfvars_path; using persisted runner settings" >&2
    return
  fi

  if value="$(read_tfvars_string "$tfvars_path" "repo_url")"; then
    REPO_URL="$value"
  fi
  if value="$(read_tfvars_string "$tfvars_path" "repo_ref")"; then
    REPO_REF="$value"
  fi
  if value="$(read_tfvars_string "$tfvars_path" "setup_script_path")"; then
    SETUP_SCRIPT_PATH="$value"
  fi
  if value="$(read_tfvars_string "$tfvars_path" "git_deploy_key_parameter_name")"; then
    DEPLOY_KEY_PARAM="$value"
  fi
  if value="$(read_tfvars_string "$tfvars_path" "cleanroom_version")"; then
    CLEANROOM_VERSION="$value"
  fi
  if value="$(read_tfvars_string "$tfvars_path" "cleanroom_install_script_ref")"; then
    CLEANROOM_INSTALL_SCRIPT_REF="$value"
  fi
  if value="$(read_tfvars_string "$tfvars_path" "tailscale_auth_key_parameter_name")"; then
    TAILSCALE_AUTH_KEY_PARAMETER_NAME="$value"
  fi
  if value="$(read_tfvars_string "$tfvars_path" "tailscale_version")"; then
    TAILSCALE_VERSION="$value"
  fi
  if value="$(read_tfvars_string "$tfvars_path" "tailscale_hostname_prefix")"; then
    TAILSCALE_HOSTNAME_PREFIX="$value"
  fi
  if value="$(read_tfvars_string "$tfvars_path" "tailscale_advertise_tags")"; then
    TAILSCALE_ADVERTISE_TAGS="$value"
  fi
  if value="$(read_tfvars_bool "$tfvars_path" "tailscale_enable_ssh")"; then
    TAILSCALE_ENABLE_SSH="$value"
  fi
  if value="$(read_tfvars_bool "$tfvars_path" "tailscale_accept_routes")"; then
    TAILSCALE_ACCEPT_ROUTES="$value"
  fi
}

: "${AWS_REGION:?AWS_REGION must be set}"
: "${NAME_PREFIX:?NAME_PREFIX must be set}"
: "${REPO_URL:?REPO_URL must be set}"
: "${REPO_REF:?REPO_REF must be set}"
: "${SETUP_SCRIPT_PATH:?SETUP_SCRIPT_PATH must be set}"
: "${INSTALL_FIRECRACKER:?INSTALL_FIRECRACKER must be set}"
: "${FIRECRACKER_VERSION:?FIRECRACKER_VERSION must be set}"
: "${CLEANROOM_BINARY_INSTALL_DIR:?CLEANROOM_BINARY_INSTALL_DIR must be set}"
: "${CLEANROOM_CONFIG_DIR:?CLEANROOM_CONFIG_DIR must be set}"
: "${HELPER_INSTALL_PATH:?HELPER_INSTALL_PATH must be set}"
: "${ZFS_DATASET:?ZFS_DATASET must be set}"

bootstrap_repo_url="$REPO_URL"
bootstrap_repo_ref="$REPO_REF"
deploy_key_path='/root/.ssh/cleanroom_deploy_key'
deploy_known_hosts='/root/.ssh/cleanroom_known_hosts'
repo_root='/opt/cleanroom-bootstrap/repo'

prepare_deploy_key() {
  if [ -z "$DEPLOY_KEY_PARAM" ]; then
    return
  fi

  if ! command -v ssh-keyscan >/dev/null 2>&1; then
    echo "ssh-keyscan is required when DEPLOY_KEY_PARAM is set" >&2
    exit 1
  fi

  install -d -o root -g root -m 0700 /root/.ssh
  retry 10 3 aws ssm get-parameter \
    --region "$AWS_REGION" \
    --name "$DEPLOY_KEY_PARAM" \
    --with-decryption \
    --query 'Parameter.Value' \
    --output text > "$deploy_key_path"
  chmod 0600 "$deploy_key_path"
  touch "$deploy_known_hosts"
  retry 5 3 ssh-keyscan -t rsa,ecdsa,ed25519 github.com >> "$deploy_known_hosts"
  chmod 0644 "$deploy_known_hosts"
  export GIT_SSH_COMMAND="ssh -i $deploy_key_path -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=$deploy_known_hosts"
}

cleanup_deploy_key() {
  unset GIT_SSH_COMMAND || true
  rm -f "$deploy_key_path" "$deploy_known_hosts"
}

fetch_repo_checkout() {
  local target_repo_url="$1"
  local target_repo_ref="$2"

  rm -rf "$repo_root"
  install -d -o root -g root -m 0755 /opt/cleanroom-bootstrap

  prepare_deploy_key

  git -c init.defaultBranch=main init "$repo_root" >/dev/null
  git -C "$repo_root" remote add origin "$target_repo_url"
  retry 5 3 git -C "$repo_root" fetch --depth=1 origin "$target_repo_ref"
  git -C "$repo_root" checkout -f FETCH_HEAD

  cleanup_deploy_key
}

if ! command -v aws >/dev/null 2>&1; then
  aws_arch='x86_64'
  if [ "$(uname -m)" = 'aarch64' ] || [ "$(uname -m)" = 'arm64' ]; then
    aws_arch='aarch64'
  fi

  aws_tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$aws_tmp_dir"' EXIT
  retry 5 5 curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-$aws_arch.zip" -o "$aws_tmp_dir/awscliv2.zip"
  unzip -q "$aws_tmp_dir/awscliv2.zip" -d "$aws_tmp_dir"
  "$aws_tmp_dir/aws/install" --bin-dir /usr/local/bin --install-dir /usr/local/aws-cli --update
fi

if ! command -v aws >/dev/null 2>&1; then
  echo "aws CLI is required but not installed" >&2
  exit 1
fi

if ! command -v git >/dev/null 2>&1; then
  echo "git is required but not installed" >&2
  exit 1
fi

imds_token="$(retry 10 3 curl -fsS -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 21600")"
instance_id="$(retry 10 3 curl -fsS -H "X-aws-ec2-metadata-token: $imds_token" http://169.254.169.254/latest/meta-data/instance-id)"

fetch_repo_checkout "$bootstrap_repo_url" "$bootstrap_repo_ref"

apply_prod_tfvars_overrides "$repo_root"

if [ "$REPO_URL" != "$bootstrap_repo_url" ] || [ "$REPO_REF" != "$bootstrap_repo_ref" ]; then
  fetch_repo_checkout "$REPO_URL" "$REPO_REF"
fi

setup_script="$repo_root/$SETUP_SCRIPT_PATH"
if [ ! -f "$setup_script" ]; then
  echo "setup script not found: $SETUP_SCRIPT_PATH" >&2
  exit 1
fi

chmod +x "$setup_script"

export AWS_REGION
export CLEANROOM_BOOTSTRAP_REGION="$AWS_REGION"
export CLEANROOM_BOOTSTRAP_INSTANCE_ID="$instance_id"
export CLEANROOM_BOOTSTRAP_NAME_PREFIX="$NAME_PREFIX"
export CLEANROOM_BOOTSTRAP_REPO_URL="$REPO_URL"
export CLEANROOM_BOOTSTRAP_REPO_REF="$REPO_REF"
export CLEANROOM_BOOTSTRAP_SETUP_SCRIPT_PATH="$SETUP_SCRIPT_PATH"
export CLEANROOM_BOOTSTRAP_DEPLOY_KEY_PARAM="$DEPLOY_KEY_PARAM"
export CLEANROOM_INSTALL_FIRECRACKER="$INSTALL_FIRECRACKER"
export CLEANROOM_FIRECRACKER_VERSION="$FIRECRACKER_VERSION"
export CLEANROOM_VERSION="$CLEANROOM_VERSION"
export CLEANROOM_INSTALL_SCRIPT_REF="$CLEANROOM_INSTALL_SCRIPT_REF"
export CLEANROOM_BINARY_INSTALL_DIR="$CLEANROOM_BINARY_INSTALL_DIR"
export CLEANROOM_CONFIG_DIR="$CLEANROOM_CONFIG_DIR"
export CLEANROOM_HELPER_INSTALL_PATH="$HELPER_INSTALL_PATH"
export CLEANROOM_TAILSCALE_AUTH_KEY_PARAMETER_NAME="$TAILSCALE_AUTH_KEY_PARAMETER_NAME"
export CLEANROOM_TAILSCALE_VERSION="$TAILSCALE_VERSION"
export CLEANROOM_TAILSCALE_HOSTNAME_PREFIX="$TAILSCALE_HOSTNAME_PREFIX"
export CLEANROOM_TAILSCALE_ADVERTISE_TAGS="$TAILSCALE_ADVERTISE_TAGS"
export CLEANROOM_TAILSCALE_ENABLE_SSH="$TAILSCALE_ENABLE_SSH"
export CLEANROOM_TAILSCALE_ACCEPT_ROUTES="$TAILSCALE_ACCEPT_ROUTES"
export CLEANROOM_ZFS_DATASET="$ZFS_DATASET"

"$setup_script"
EOF
  chmod 0755 "$bootstrap_runner_path"
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

  if ! tailscale_auth_key="$(retry 10 3 aws ssm get-parameter --region "$AWS_REGION" --name "$tailscale_param" --with-decryption --query 'Parameter.Value' --output text)"; then
    warn "tailscale auth key parameter unavailable (${tailscale_param}); skipping tailscale bootstrap"
    return
  fi

  log "installing tailscale ${tailscale_version}"

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

install_cleanroom_release() {
  local install_script_repo="$1"
  local install_script_ref="$2"
  local release_repo="$3"
  local release_version="$4"
  local install_script_url
  local tmp_dir
  local install_script_path

  install_script_url="$(raw_github_url "$install_script_repo" "$install_script_ref" "scripts/install.sh")"
  log "installing cleanroom release ${release_repo} (${release_version})"
  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' RETURN
  install_script_path="$tmp_dir/install.sh"

  retry 5 5 curl -fsSL "$install_script_url" -o "$install_script_path"
  chmod 0755 "$install_script_path"

  if [ "$release_version" = "latest" ]; then
    retry 5 5 env \
      CLEANROOM_REPO="$release_repo" \
      CLEANROOM_INSTALL_DIR="$CLEANROOM_BINARY_INSTALL_DIR" \
      bash "$install_script_path"
    return
  fi

  retry 5 5 env \
    CLEANROOM_REPO="$release_repo" \
    CLEANROOM_VERSION="$release_version" \
    CLEANROOM_INSTALL_DIR="$CLEANROOM_BINARY_INSTALL_DIR" \
    bash "$install_script_path"
}

installed_cleanroom_version() {
  local version
  version="$("$CLEANROOM_BINARY_INSTALL_DIR/cleanroom" version | awk '{print $3}')"
  [ -n "$version" ] || die "failed to determine installed cleanroom version"
  printf '%s' "$version"
}

install_cleanroom_root_helper() {
  local release_repo="$1"
  local release_version="$2"
  local helper_url
  local tmp_dir
  local helper_source_path

  helper_url="$(raw_github_url "$release_repo" "$release_version" "scripts/cleanroom-root-helper.sh")"
  log "installing cleanroom-root-helper ${release_version}"

  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' RETURN
  helper_source_path="$tmp_dir/cleanroom-root-helper"
  retry 5 5 curl -fsSL "$helper_url" -o "$helper_source_path"

  install -d -o root -g root -m 0755 "$(dirname "$HELPER_INSTALL_PATH")"
  install -o root -g root -m 0755 "$helper_source_path" "$HELPER_INSTALL_PATH"
}

if [ "$(id -u)" -ne 0 ]; then
  die "must run as root"
fi

NAME_PREFIX="${CLEANROOM_BOOTSTRAP_NAME_PREFIX:-cleanroom-prod}"
INSTALL_FIRECRACKER="${CLEANROOM_INSTALL_FIRECRACKER:-true}"
FIRECRACKER_VERSION="${CLEANROOM_FIRECRACKER_VERSION:-1.14.2}"
CLEANROOM_BINARY_INSTALL_DIR="${CLEANROOM_BINARY_INSTALL_DIR:-/usr/local/bin}"
CLEANROOM_CONFIG_DIR="${CLEANROOM_CONFIG_DIR:-/root/.config/cleanroom}"
CLEANROOM_VERSION="${CLEANROOM_VERSION:-v0.3.0}"
CLEANROOM_INSTALL_SCRIPT_REF="${CLEANROOM_INSTALL_SCRIPT_REF:-main}"
CLEANROOM_FIRECRACKER_VCPUS="${CLEANROOM_FIRECRACKER_VCPUS:-4}"
CLEANROOM_FIRECRACKER_MEMORY_MIB="${CLEANROOM_FIRECRACKER_MEMORY_MIB:-8192}"
CLEANROOM_FIRECRACKER_LAUNCH_SECONDS="${CLEANROOM_FIRECRACKER_LAUNCH_SECONDS:-90}"
HELPER_INSTALL_PATH="${CLEANROOM_HELPER_INSTALL_PATH:-/usr/local/sbin/cleanroom-root-helper}"
AWS_REGION="${AWS_REGION:-${CLEANROOM_BOOTSTRAP_REGION:-}}"
BOOTSTRAP_REPO_URL="${CLEANROOM_BOOTSTRAP_REPO_URL:-git@github.com:buildkite/cleanroom.git}"
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

require_cmd apt-get
require_cmd awk
require_cmd curl
require_cmd tar
require_cmd systemctl

install_sudo_if_missing
install_tailscale_if_configured

RELEASE_REPO="${CLEANROOM_REPO:-}"
if [ -z "$RELEASE_REPO" ]; then
  RELEASE_REPO="$(github_repo_slug_from_url "$BOOTSTRAP_REPO_URL" 2>/dev/null || true)"
fi
[ -n "$RELEASE_REPO" ] || RELEASE_REPO="buildkite/cleanroom"
CLEANROOM_VERSION="$(normalize_version "$CLEANROOM_VERSION")"

configure_apt_ipv4
export DEBIAN_FRONTEND=noninteractive
retry 5 10 apt-get update -y
retry 5 10 apt-get install -y e2fsprogs iproute2 iptables

if [ "$INSTALL_FIRECRACKER" = "true" ]; then
  log "installing firecracker ${FIRECRACKER_VERSION}"
  install_firecracker_binary "$FIRECRACKER_VERSION"
fi

install -d -o root -g root -m 0755 "$XDG_CONFIG_HOME" "$XDG_STATE_HOME" "$XDG_DATA_HOME"
install -d -o root -g root -m 0755 "$CLEANROOM_BINARY_INSTALL_DIR"

install_cleanroom_release "$RELEASE_REPO" "$CLEANROOM_INSTALL_SCRIPT_REF" "$RELEASE_REPO" "$CLEANROOM_VERSION"
INSTALLED_CLEANROOM_VERSION="$(installed_cleanroom_version)"
install_cleanroom_root_helper "$RELEASE_REPO" "$(normalize_version "$INSTALLED_CLEANROOM_VERSION")"

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
    privileged_helper_path: ${HELPER_INSTALL_PATH}
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

install_host_bootstrap_runner

log "running cleanroom doctor"
"$CLEANROOM_BINARY_INSTALL_DIR/cleanroom" doctor

log "installing cleanroom system daemon"
"$CLEANROOM_BINARY_INSTALL_DIR/cleanroom" daemon install --force --log-level info

systemctl is-active --quiet cleanroom.service || die "cleanroom.service failed to start"

log "cleanroom host ready (${NAME_PREFIX})"
