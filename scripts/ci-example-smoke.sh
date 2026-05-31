#!/usr/bin/env bash
set -euo pipefail

BACKEND="${1:-}"
LISTEN_ENDPOINT="${2:-}"
REPO_ROOT="${3:-}"

if [[ -z "$BACKEND" || -z "$LISTEN_ENDPOINT" || -z "$REPO_ROOT" ]]; then
  echo "usage: $0 <backend> <listen-endpoint> <repo-root>" >&2
  exit 1
fi

if ! command -v gotestsum >/dev/null 2>&1; then
  echo "gotestsum is required; install it with mise before running example smoke tests" >&2
  exit 127
fi

cd "$REPO_ROOT"

mkdir -p tmp/test-engine

result_path="${BUILDKITE_TEST_ENGINE_RESULT_PATH:-tmp/test-engine/examples-${BACKEND}.xml}"
timeout="${CLEANROOM_CI_EXAMPLE_TEST_TIMEOUT:-45m}"

export CLEANROOM_CI_EXAMPLE_ENABLED=1
export CLEANROOM_CI_EXAMPLE_BACKEND="$BACKEND"
export CLEANROOM_CI_EXAMPLE_LISTEN_ENDPOINT="$LISTEN_ENDPOINT"
export CLEANROOM_CI_EXAMPLE_REPO_ROOT="$REPO_ROOT"

gotestsum \
  --format testname \
  --junitfile "$result_path" \
  -- \
  -run '^TestCIExampleSmoke$' \
  -count=1 \
  -timeout "$timeout" \
  ./scripts
