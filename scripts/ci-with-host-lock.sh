#!/usr/bin/env bash
set -euo pipefail

SCRIPT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"

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
  local status

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

  set +e
  (
    cd "$workspace_dir" && "$@"
  )
  status=$?
  set -e

  rm -rf "$workspace_dir"
  return "$status"
}

if [[ "${1:-}" == "--internal-run-wrapped" ]]; then
  shift
  run_wrapped_command "$@"
  exit $?
fi

run_with_file_lock() {
  local lock_dir="${CLEANROOM_CI_HOST_LOCK_DIR:-/tmp/cleanroom-ci-host-locks}"
  local lock_file="$lock_dir/$safe_lock_key.lock"
  local lock_fd
  local status

  mkdir -p "$lock_dir"
  chmod 1777 "$lock_dir" 2>/dev/null || true
  touch "$lock_file"
  chmod 666 "$lock_file" 2>/dev/null || true

  if command -v flock >/dev/null 2>&1; then
    echo "--- :lock: Acquire host file lock ($lock_key at $lock_file)"
    exec {lock_fd}>"$lock_file"
    flock "$lock_fd"
    set +e
    run_wrapped_command "$@"
    status=$?
    set -e
    flock -u "$lock_fd" || true
    exec {lock_fd}>&-
    return "$status"
  fi

  if command -v lockf >/dev/null 2>&1; then
    echo "--- :lock: Acquire host file lock ($lock_key at $lock_file)"
    lockf "$lock_file" "$SCRIPT_PATH" --internal-run-wrapped "$@"
    return $?
  fi

  echo "buildkite-agent lock failed and no host file-lock command is available for: $lock_key" >&2
  return 127
}

if [[ "$#" -lt 2 ]]; then
  usage
  exit 64
fi

lock_key="$1"
shift

safe_lock_key="${lock_key//[^A-Za-z0-9_.-]/_}"
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

if command -v buildkite-agent >/dev/null 2>&1; then
  buildkite_lock_wait_timeout="${CLEANROOM_BUILDKITE_LOCK_WAIT_TIMEOUT:-${BUILDKITE_LOCK_WAIT_TIMEOUT:-45m}}"
  echo "--- :lock: Acquire Buildkite host lock ($lock_key)"
  acquire_err="$(mktemp "${TMPDIR:-/tmp}/cleanroom-host-lock.XXXXXX")"
  trap release_buildkite_lock EXIT

  set +e
  token="$(buildkite-agent lock acquire --lock-wait-timeout "$buildkite_lock_wait_timeout" "$lock_key" 2>"$acquire_err")"
  acquire_status=$?
  set -e

  if [[ "$acquire_status" -eq 0 ]]; then
    rm -f "$acquire_err"
    run_wrapped_command "$@"
    exit $?
  fi

  trap - EXIT
  token=""
  if grep -Eiq 'timeout|timed out|deadline' "$acquire_err"; then
    cat "$acquire_err" >&2 || true
    rm -f "$acquire_err"
    exit "$acquire_status"
  fi

  echo "buildkite-agent lock acquire failed; falling back to host file lock" >&2
  cat "$acquire_err" >&2 || true
  rm -f "$acquire_err"
fi

run_with_file_lock "$@"
