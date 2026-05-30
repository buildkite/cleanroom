#!/usr/bin/env bash

set -euo pipefail

die() {
  echo "ci-auth-oidc-smoke: $*" >&2
  exit 1
}

require_command() {
  local name="$1"
  command -v "$name" >/dev/null 2>&1 || die "$name is required"
}

jwt_claim() {
  local claim="$1"
  python3 - "$claims_path" "$claim" <<'PY'
import json
import sys

claims_path, claim = sys.argv[1], sys.argv[2]
with open(claims_path, encoding="utf-8") as fh:
    claims = json.load(fh)

value = claims.get(claim)
if value is None or value == "":
    raise SystemExit(f"missing {claim} claim")
if not isinstance(value, str):
    raise SystemExit(f"{claim} claim must be a string")

print(value)
PY
}

assert_safe_identifier() {
  local name="$1"
  local value="$2"

  [[ "$value" =~ ^[A-Za-z0-9._:-]+$ ]] || die "$name contains unexpected characters"
}

assert_safe_audience() {
  local value="$1"

  [[ "$value" =~ ^[A-Za-z0-9._:/-]+$ ]] || die "audience contains unexpected characters"
}

expect_auth_check() {
  local label="$1"
  local expected_allowed="$2"
  shift 2

  decision_index=$((decision_index + 1))
  local output_path="$tmpdir/decision-${decision_index}.json"
  local error_path="$tmpdir/decision-${decision_index}.stderr"
  local status

  set +e
  "$cleanroom_bin" auth check \
    --config "$config_path" \
    --token-file "$token_path" \
    "$@" \
    --json \
    >"$output_path" \
    2>"$error_path"
  status=$?
  set -e

  if [[ "$expected_allowed" == "true" && "$status" -ne 0 ]]; then
    cat "$error_path" >&2
    cat "$output_path" >&2
    die "$label should have been allowed"
  fi
  if [[ "$expected_allowed" == "false" && "$status" -ne 1 ]]; then
    cat "$error_path" >&2
    cat "$output_path" >&2
    die "$label should have been denied"
  fi

  python3 - "$output_path" "$expected_allowed" "$label" <<'PY'
import json
import sys

output_path, expected, label = sys.argv[1], sys.argv[2] == "true", sys.argv[3]
with open(output_path, encoding="utf-8") as fh:
    decision = json.load(fh)

if decision.get("allowed") is not expected:
    raise SystemExit(f"{label}: expected allowed={expected}, got {decision.get('allowed')}")
PY

  echo "auth check passed: $label"
}

require_command buildkite-agent
require_command go
require_command python3

audience="${CLEANROOM_AUTH_OIDC_AUDIENCE:-https://cleanroom.buildkite.com/ci-auth-smoke}"
assert_safe_audience "$audience"
tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/cleanroom-auth-oidc.XXXXXXXXXX")"
cleanup() {
  rm -rf "$tmpdir"
}
trap cleanup EXIT

cleanroom_bin="$tmpdir/cleanroom"
token_path="$tmpdir/buildkite.jwt"
claims_path="$tmpdir/claims.json"
config_path="$tmpdir/config.yaml"
allowed_request_path="$tmpdir/create-allowed.json"
denied_request_path="$tmpdir/create-denied.json"
decision_index=0

go build -o "$cleanroom_bin" ./cmd/cleanroom

buildkite-agent oidc request-token \
  --audience "$audience" \
  --lifetime 300 \
  --subject-claim pipeline_id \
  --claim organization_id,pipeline_id \
  >"$token_path"
chmod 0600 "$token_path"
[[ -s "$token_path" ]] || die "Buildkite OIDC token request produced an empty token"

python3 - "$token_path" >"$claims_path" <<'PY'
import base64
import json
import pathlib
import sys

token = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8").strip()
parts = token.split(".")
if len(parts) != 3:
    raise SystemExit("token is not a compact JWT")

payload = parts[1] + "=" * (-len(parts[1]) % 4)
claims = json.loads(base64.urlsafe_b64decode(payload.encode("ascii")))
json.dump(claims, sys.stdout, sort_keys=True)
PY

organization_id="$(jwt_claim organization_id)"
pipeline_id="$(jwt_claim pipeline_id)"
expected_organization_id="${CLEANROOM_AUTH_OIDC_ORGANIZATION_ID:-$organization_id}"
expected_pipeline_id="${CLEANROOM_AUTH_OIDC_PIPELINE_ID:-$pipeline_id}"

assert_safe_identifier organization_id "$organization_id"
assert_safe_identifier pipeline_id "$pipeline_id"
assert_safe_identifier expected_organization_id "$expected_organization_id"
assert_safe_identifier expected_pipeline_id "$expected_pipeline_id"

[[ "$organization_id" == "$expected_organization_id" ]] || die "organization_id claim did not match CLEANROOM_AUTH_OIDC_ORGANIZATION_ID"
[[ "$pipeline_id" == "$expected_pipeline_id" ]] || die "pipeline_id claim did not match CLEANROOM_AUTH_OIDC_PIPELINE_ID"

owner_principal_id="oidc:buildkite:org:${organization_id}:pipeline:${pipeline_id}"
owner_scope="org:${organization_id}"
other_principal_id="oidc:buildkite:org:${organization_id}:pipeline:other-pipeline"

cat >"$config_path" <<YAML
default_backend: firecracker
auth:
  required: true
  oidc:
    issuers:
      - name: buildkite
        issuer: https://agent.buildkite.com
        audiences:
          - "$audience"
        jwks_url: https://agent.buildkite.com/.well-known/jwks
        required_claims:
          organization_id: "$expected_organization_id"
        clock_skew_seconds: 60
        max_token_lifetime_seconds: 600
  policy:
    bindings:
      - name: buildkite-ci
        when: >
          token.issuer == "buildkite" &&
          claims.pipeline_id == "$expected_pipeline_id"
        principal:
          id: "oidc:\${token.issuer}:org:\${claims.organization_id}:pipeline:\${claims.pipeline_id}"
          scope: "org:\${claims.organization_id}"
        grants:
          - name: create-cleanroom-sandbox
            actions: [sandbox.create]
            resources: [sandbox]
            condition: >
              request.repository.remote_url == "https://github.com/buildkite/cleanroom.git" &&
              request.backend == "firecracker" &&
              request.policy.resources.vcpus <= 4 &&
              request.policy.resources.memory_bytes <= 8589934592 &&
              request.policy.docker.required == false &&
              request.policy.network_default == "deny"
          - name: manage-owned-sandboxes
            actions: [sandbox.get]
            resources: [sandbox]
            # auth check only sees supplied owner metadata, so keep this grant
            # owner-scoped to exercise existing-resource diagnostics locally.
            condition: >
              resource.owner.principal_id == principal.id &&
              resource.owner.scope == principal.scope
YAML

cat >"$allowed_request_path" <<'JSON'
{
  "repository": {"remote_url": "https://github.com/buildkite/cleanroom.git"},
  "backend": "firecracker",
  "policy": {
    "resources": {"vcpus": 4, "memory_bytes": 8589934592},
    "docker": {"required": false},
    "network_default": "deny"
  }
}
JSON

cat >"$denied_request_path" <<'JSON'
{
  "repository": {"remote_url": "https://github.com/buildkite/other.git"},
  "backend": "firecracker",
  "policy": {
    "resources": {"vcpus": 4, "memory_bytes": 8589934592},
    "docker": {"required": false},
    "network_default": "deny"
  }
}
JSON

expect_auth_check "allowed sandbox create" true \
  --action sandbox.create \
  --request "$allowed_request_path"

expect_auth_check "denied sandbox create for another repository" false \
  --action sandbox.create \
  --request "$denied_request_path"

expect_auth_check "allowed same-owner sandbox get" true \
  --action sandbox.get \
  --resource-id sbx_auth_oidc_smoke \
  --owner-principal-id "$owner_principal_id" \
  --owner-scope "$owner_scope"

expect_auth_check "denied cross-principal sandbox get" false \
  --action sandbox.get \
  --resource-id sbx_auth_oidc_smoke \
  --owner-principal-id "$other_principal_id" \
  --owner-scope "$owner_scope"

echo "Buildkite OIDC auth smoke passed for organization_id=$organization_id pipeline_id=$pipeline_id"
