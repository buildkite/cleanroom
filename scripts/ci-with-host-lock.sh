#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 <lock-key> <command> [args...]" >&2
}

if [[ "$#" -lt 2 ]]; then
  usage
  exit 64
fi

lock_key="$1"
shift

safe_lock_key="${lock_key//[^A-Za-z0-9_.-]/_}"

run_with_file_lock() {
  local lock_dir="${TMPDIR:-/tmp}/cleanroom-ci-host-locks"
  local lock_file="$lock_dir/$safe_lock_key.lock"

  mkdir -p "$lock_dir"

  if command -v flock >/dev/null 2>&1; then
    echo "--- :lock: Acquire host file lock ($lock_key)"
    flock "$lock_file" "$@"
    return $?
  fi

  if command -v lockf >/dev/null 2>&1; then
    echo "--- :lock: Acquire host file lock ($lock_key)"
    lockf "$lock_file" "$@"
    return $?
  fi

  echo "buildkite-agent lock failed and no host file-lock command is available for: $lock_key" >&2
  return 127
}

release_buildkite_lock() {
  local status=$?
  if [[ -n "${token:-}" ]]; then
    echo "--- :unlock: Release host lock ($lock_key)"
    buildkite-agent lock release "$lock_key" "$token" || true
  fi
  exit "$status"
}

if command -v buildkite-agent >/dev/null 2>&1; then
  echo "--- :lock: Acquire Buildkite host lock ($lock_key)"
  acquire_err="$(mktemp "${TMPDIR:-/tmp}/cleanroom-host-lock.XXXXXX")"
  if token="$(buildkite-agent lock acquire "$lock_key" 2>"$acquire_err")"; then
    rm -f "$acquire_err"
    trap release_buildkite_lock EXIT
    "$@"
    exit $?
  fi

  echo "buildkite-agent lock acquire failed; falling back to host file lock" >&2
  cat "$acquire_err" >&2 || true
  rm -f "$acquire_err"
fi

run_with_file_lock "$@"
