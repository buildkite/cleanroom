#!/usr/bin/env bash
set -euo pipefail

readonly DEFAULT_CI_AWS_PROFILE="buildkite-sandbox-pipelines-admin"
readonly DEFAULT_CI_AWS_REGION="ap-southeast-2"
readonly DEFAULT_CI_TERRAFORM_DIR="infra/terraform/envs/ci"

usage() {
  cat >&2 <<'EOF'
usage: scripts/ci-bootstrap-linux-ssm.sh <run|logs>

Environment overrides:
  CLEANROOM_CI_AWS_PROFILE   AWS profile for the SSM command (default: buildkite-sandbox-pipelines-admin)
  CLEANROOM_CI_AWS_REGION    AWS region for the SSM command (default: ap-southeast-2)
  CLEANROOM_CI_INSTANCE_ID   Explicit linux CI instance id (default: terraform output)
  CLEANROOM_CI_TERRAFORM_DIR Terraform dir used to resolve instance_id (default: infra/terraform/envs/ci)
EOF
  exit 1
}

require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || {
    echo "required command not found: $cmd" >&2
    exit 1
  }
}

resolve_instance_id() {
  local terraform_dir="$1"
  local instance_id="${CLEANROOM_CI_INSTANCE_ID:-}"

  if [ -n "$instance_id" ]; then
    printf '%s\n' "$instance_id"
    return
  fi

  require_cmd terraform
  terraform -chdir="$terraform_dir" output -raw instance_id
}

run_ssm() {
  local aws_profile="$1"
  local aws_region="$2"
  local instance_id="$3"
  local parameters="$4"
  local command_id
  local wait_status
  local status
  local response_code
  local stdout
  local stderr

  command_id="$(
    AWS_PROFILE="$aws_profile" AWS_REGION="$aws_region" \
      aws ssm send-command \
        --instance-ids "$instance_id" \
        --document-name AWS-RunShellScript \
        --parameters "$parameters" \
        --query 'Command.CommandId' \
        --output text
  )"

  printf 'command_id: %s\n' "$command_id"

  set +e
  AWS_PROFILE="$aws_profile" AWS_REGION="$aws_region" \
    aws ssm wait command-executed \
      --command-id "$command_id" \
      --instance-id "$instance_id"
  wait_status=$?
  set -e

  status="$(
    AWS_PROFILE="$aws_profile" AWS_REGION="$aws_region" \
      aws ssm get-command-invocation \
        --command-id "$command_id" \
        --instance-id "$instance_id" \
        --query 'Status' \
        --output text
  )"
  response_code="$(
    AWS_PROFILE="$aws_profile" AWS_REGION="$aws_region" \
      aws ssm get-command-invocation \
        --command-id "$command_id" \
        --instance-id "$instance_id" \
        --query 'ResponseCode' \
        --output text
  )"
  stdout="$(
    AWS_PROFILE="$aws_profile" AWS_REGION="$aws_region" \
      aws ssm get-command-invocation \
        --command-id "$command_id" \
        --instance-id "$instance_id" \
        --query 'StandardOutputContent' \
        --output text
  )"
  stderr="$(
    AWS_PROFILE="$aws_profile" AWS_REGION="$aws_region" \
      aws ssm get-command-invocation \
        --command-id "$command_id" \
        --instance-id "$instance_id" \
        --query 'StandardErrorContent' \
        --output text
  )"

  printf 'status: %s\n' "$status"
  printf 'response_code: %s\n' "$response_code"

  if [ -n "$stdout" ] && [ "$stdout" != "None" ]; then
    printf '%s\n%s\n' '--- stdout ---' "$stdout"
  fi

  if [ -n "$stderr" ] && [ "$stderr" != "None" ]; then
    printf '%s\n%s\n' '--- stderr ---' "$stderr" >&2
  fi

  if [ "$wait_status" -ne 0 ] || [ "$status" != "Success" ]; then
    exit 1
  fi
}

main() {
  local mode="${1:-}"
  local aws_profile="${CLEANROOM_CI_AWS_PROFILE:-$DEFAULT_CI_AWS_PROFILE}"
  local aws_region="${CLEANROOM_CI_AWS_REGION:-$DEFAULT_CI_AWS_REGION}"
  local terraform_dir="${CLEANROOM_CI_TERRAFORM_DIR:-$DEFAULT_CI_TERRAFORM_DIR}"
  local instance_id
  local parameters

  require_cmd aws

  case "$mode" in
    run)
      parameters='{"commands":["sudo env PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin /usr/local/bin/cleanroom-bootstrap-linux"]}'
      ;;
    logs)
      parameters='{"commands":["sudo tail -n 120 /var/log/cleanroom-bootstrap-linux.log","sudo systemctl status buildkite-agent@cleanroom.service --no-pager","sudo tail -n 120 /var/lib/buildkite-agent/logs/buildkite-agent-cleanroom.log"]}'
      ;;
    *)
      usage
      ;;
  esac

  instance_id="$(resolve_instance_id "$terraform_dir")"
  if [ -z "$instance_id" ]; then
    echo "failed to resolve linux CI instance_id" >&2
    exit 1
  fi

  printf 'instance_id: %s\n' "$instance_id"
  run_ssm "$aws_profile" "$aws_region" "$instance_id" "$parameters"
}

main "$@"
