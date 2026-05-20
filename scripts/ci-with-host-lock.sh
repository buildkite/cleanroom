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

if ! command -v buildkite-agent >/dev/null 2>&1; then
  echo "buildkite-agent is required to acquire host lock: $lock_key" >&2
  exit 127
fi

echo "--- :lock: Acquire host lock ($lock_key)"
token="$(buildkite-agent lock acquire "$lock_key")"

cleanup() {
  local status=$?
  if [[ -n "${token:-}" ]]; then
    echo "--- :unlock: Release host lock ($lock_key)"
    buildkite-agent lock release "$lock_key" "$token" || true
  fi
  exit "$status"
}
trap cleanup EXIT

"$@"
