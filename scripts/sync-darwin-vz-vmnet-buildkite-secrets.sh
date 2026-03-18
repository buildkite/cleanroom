#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  BUILDKITE_CLUSTER_ID=<cluster-id> \
  CLEANROOM_DARWIN_VZ_HELPER_CERT_P12_PATH=/path/to/cert.p12 \
  CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE_PATH=/path/to/profile.provisionprofile \
  CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY='Apple Development: ...' \
  scripts/sync-darwin-vz-vmnet-buildkite-secrets.sh

Required environment:
  BUILDKITE_CLUSTER_ID or BK_CLUSTER_ID
  CLEANROOM_DARWIN_VZ_HELPER_CERT_P12_PATH
  CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE_PATH
  CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY

Optional environment:
  CLEANROOM_DARWIN_VZ_HELPER_CERT_PASSWORD
    If omitted, the script deletes and recreates the password secret and lets
    `bk secret create` prompt for the value interactively.
    Buildkite cluster secrets cannot be empty, so export the `.p12` with a
    non-empty password.
  BUILDKITE_SECRET_POLICY
    Optional YAML access policy applied to every created secret.
  BUILDKITE_PIPELINE_ID and BUILDKITE_CLUSTER_QUEUE_ID
    Optional first-party Buildkite claims used to synthesize a YAML access
    policy when BUILDKITE_SECRET_POLICY is not set.
EOF
}

fail() {
  echo "$*" >&2
  exit 1
}

require_command() {
  local command_name="$1"
  command -v "$command_name" >/dev/null 2>&1 || fail "missing required command: $command_name"
}

require_file() {
  local path="$1"
  local label="$2"
  [[ -f "$path" ]] || fail "missing ${label}: $path"
}

base64_no_wrap() {
  base64 < "$1" | tr -d '\n'
}

cluster_id="${BUILDKITE_CLUSTER_ID:-${BK_CLUSTER_ID:-}}"
p12_path="${CLEANROOM_DARWIN_VZ_HELPER_CERT_P12_PATH:-}"
profile_path="${CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE_PATH:-}"
sign_identity="${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY:-}"
cert_password="${CLEANROOM_DARWIN_VZ_HELPER_CERT_PASSWORD:-}"
cert_password_is_set=0
if [[ "${CLEANROOM_DARWIN_VZ_HELPER_CERT_PASSWORD+x}" == "x" ]]; then
  cert_password_is_set=1
fi
secret_policy="${BUILDKITE_SECRET_POLICY:-}"
pipeline_id="${BUILDKITE_PIPELINE_ID:-}"
cluster_queue_id="${BUILDKITE_CLUSTER_QUEUE_ID:-}"

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  usage
  exit 0
fi
if [[ $# -ne 0 ]]; then
  usage >&2
  exit 1
fi

[[ -n "$cluster_id" ]] || fail "BUILDKITE_CLUSTER_ID (or BK_CLUSTER_ID) is required"
[[ -n "$p12_path" ]] || fail "CLEANROOM_DARWIN_VZ_HELPER_CERT_P12_PATH is required"
[[ -n "$profile_path" ]] || fail "CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE_PATH is required"
[[ -n "$sign_identity" ]] || fail "CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY is required"

require_command bk
require_command ruby
require_command base64
require_file "$p12_path" "Apple Development .p12"
require_file "$profile_path" "development provisioning profile"
if [[ "$cert_password_is_set" -eq 1 && -z "$cert_password" ]]; then
  fail "Buildkite cluster secrets cannot store an empty p12 password. Re-export ${p12_path} with a non-empty password and retry."
fi
if [[ -z "$secret_policy" && -n "$pipeline_id" && -n "$cluster_queue_id" ]]; then
  secret_policy=$(printf -- '- pipeline_id: "%s"\n  cluster_queue_id: "%s"' "$pipeline_id" "$cluster_queue_id")
fi

list_secrets_json() {
  local output
  if ! output="$(bk secret list --cluster-id "$cluster_id" -o json 2>&1)"; then
    if [[ "$output" == *"read_secrets_details"* || "$output" == *"read_secret_details"* ]]; then
      fail "bk auth token is missing Buildkite secret scopes. Re-run \`bk auth login --scopes 'read_user read_organizations read_pipelines read_builds write_builds read_agents read_artifacts read_clusters read_teams read_secrets_details write_secrets'\` and retry."
    fi
    echo "$output" >&2
    fail "failed to list existing Buildkite secrets"
  fi
  printf '%s' "$output"
}

secret_id_for_key() {
  local key="$1"

  list_secrets_json | ruby -rjson -e '
    key = ARGV.fetch(0)
    payload = JSON.parse(STDIN.read)
    find_secret = lambda do |node|
      case node
      when Array
        node.each do |child|
          found = find_secret.call(child)
          return found if found
        end
      when Hash
        return node if node["key"] == key && (node["id"] || node["uuid"])
        node.each_value do |child|
          found = find_secret.call(child)
          return found if found
        end
      end
      nil
    end
    secret = find_secret.call(payload)
    id = secret && (secret["id"] || secret["uuid"])
    puts id if id && !id.empty?
  ' "$key"
}

delete_secret_if_present() {
  local key="$1"
  local secret_id
  secret_id="$(secret_id_for_key "$key")"
  if [[ -n "$secret_id" ]]; then
    echo "Replacing existing Buildkite secret: $key"
    bk secret delete --cluster-id "$cluster_id" --secret-id "$secret_id" --yes
  fi
}

upsert_secret_value() {
  local key="$1"
  local description="$2"
  local value="$3"
  local -a create_args=(
    --cluster-id "$cluster_id"
    --key "$key"
    --value "$value"
    --description "$description"
  )

  if [[ -n "$secret_policy" ]]; then
    create_args+=(--policy="$secret_policy")
  fi

  delete_secret_if_present "$key"
  echo "Creating Buildkite secret: $key"
  bk secret create "${create_args[@]}"
}

upsert_secret_password() {
  local key="$1"
  local description="$2"
  local -a create_args=(
    --cluster-id "$cluster_id"
    --key "$key"
    --description "$description"
  )

  if [[ -n "$secret_policy" ]]; then
    create_args+=(--policy="$secret_policy")
  fi

  delete_secret_if_present "$key"
  echo "Creating Buildkite secret: $key"
  if [[ "$cert_password_is_set" -eq 1 ]]; then
    bk secret create "${create_args[@]}" --value "$cert_password"
    return
  fi
  bk secret create "${create_args[@]}"
}

upsert_secret_value \
  "CLEANROOM_DARWIN_VZ_HELPER_CERT_P12_BASE64" \
  "Base64-encoded Apple Development .p12 for cleanroom darwin-vz vmnet CI" \
  "$(base64_no_wrap "$p12_path")"

upsert_secret_password \
  "CLEANROOM_DARWIN_VZ_HELPER_CERT_PASSWORD" \
  "Password for cleanroom darwin-vz Apple Development .p12"

upsert_secret_value \
  "CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE_BASE64" \
  "Base64-encoded macOS development provisioning profile for com.buildkite.cleanroom.darwin-vz" \
  "$(base64_no_wrap "$profile_path")"

upsert_secret_value \
  "CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY" \
  "codesign identity string for cleanroom darwin-vz vmnet CI" \
  "$sign_identity"

echo "Synced darwin-vz vmnet Buildkite secrets for cluster $cluster_id"
echo "Set CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER=com.buildkite.cleanroom.darwin-vz separately in pipeline or step environment."
