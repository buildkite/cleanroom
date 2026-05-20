#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 <lock-key> <command> [args...]" >&2
}

workspace_isolation_enabled() {
  case "${CLEANROOM_CI_ISOLATE_WORKSPACE:-auto}" in
    0 | false | no)
      return 1
      ;;
    1 | true | yes)
      return 0
      ;;
    auto | "")
      [[ -n "${BUILDKITE:-}" ]]
      ;;
    *)
      echo "unsupported CLEANROOM_CI_ISOLATE_WORKSPACE value: $CLEANROOM_CI_ISOLATE_WORKSPACE" >&2
      return 2
      ;;
  esac
}

run_wrapped_command() {
  local isolate_status
  set +e
  workspace_isolation_enabled
  isolate_status=$?
  set -e

  if [[ "$isolate_status" -eq 1 ]]; then
    "$@"
    return $?
  fi
  if [[ "$isolate_status" -ne 0 ]]; then
    return "$isolate_status"
  fi

  local workspace_parent="${CLEANROOM_CI_WORKSPACE_PARENT:-${TMPDIR:-/tmp}}"
  local workspace_dir
  local source_origin_url
  local status

  source_origin_url="$(git config --get remote.origin.url || true)"

  mkdir -p "$workspace_parent"
  workspace_dir="$(mktemp -d "$workspace_parent/cleanroom-ci-workspace.XXXXXX")"
  echo "--- :file_folder: Isolate CI workspace ($workspace_dir)"

  set +e
  git clone --local --no-hardlinks --quiet "$PWD" "$workspace_dir"
  status=$?
  set -e
  if [[ "$status" -ne 0 ]]; then
    rm -rf "$workspace_dir"
    return "$status"
  fi

  set +e
  git -C "$workspace_dir" checkout --detach --quiet HEAD
  status=$?
  set -e
  if [[ "$status" -ne 0 ]]; then
    rm -rf "$workspace_dir"
    return "$status"
  fi

  if [[ -n "$source_origin_url" ]]; then
    set +e
    git -C "$workspace_dir" remote set-url origin "$source_origin_url"
    status=$?
    set -e
    if [[ "$status" -ne 0 ]]; then
      rm -rf "$workspace_dir"
      return "$status"
    fi
  fi

  set +e
  (
    cd "$workspace_dir" && "$@"
  )
  status=$?
  set -e

  rm -rf "$workspace_dir"
  return "$status"
}

if [[ "$#" -lt 2 ]]; then
  usage
  exit 64
fi

lock_key="$1"
shift

buildkite_lock_wait_timeout="${CLEANROOM_BUILDKITE_LOCK_WAIT_TIMEOUT:-${BUILDKITE_LOCK_WAIT_TIMEOUT:-45m}}"
token=""

release_buildkite_lock() {
  local status=$?
  local release_status=0

  if [[ -n "${token:-}" ]]; then
    echo "--- :unlock: Release host lock ($lock_key)"
    set +e
    buildkite-agent lock release "$lock_key" "$token"
    release_status=$?
    set -e

    if [[ "$release_status" -ne 0 ]]; then
      echo "buildkite-agent lock release failed for: $lock_key" >&2
      if [[ "$status" -eq 0 ]]; then
        status="$release_status"
      fi
    fi
  fi

  exit "$status"
}

if ! command -v buildkite-agent >/dev/null 2>&1; then
  echo "buildkite-agent is required for host locks" >&2
  exit 127
fi

echo "--- :lock: Acquire Buildkite host lock ($lock_key)"
trap release_buildkite_lock EXIT
token="$(buildkite-agent lock acquire --lock-wait-timeout "$buildkite_lock_wait_timeout" "$lock_key")"

run_wrapped_command "$@"
