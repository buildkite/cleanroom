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

"$@"
