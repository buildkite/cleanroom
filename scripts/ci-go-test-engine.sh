#!/usr/bin/env bash
set -euo pipefail

BKTEC_VERSION="${BKTEC_VERSION:-2.6.0}"

cache_dir="${XDG_CACHE_HOME:-$HOME/.cache}/cleanroom/test-engine"
bin_dir="${cache_dir}/bin"
mkdir -p "$bin_dir" tmp/test-engine

install_bktec() {
  local goos goarch asset url target

  target="${bin_dir}/bktec-${BKTEC_VERSION}"
  if [[ -x "$target" ]]; then
    ln -sf "$target" "${bin_dir}/bktec"
    return
  fi

  goos="$(go env GOOS)"
  goarch="$(go env GOARCH)"
  asset="bktec_${BKTEC_VERSION}_${goos}_${goarch}"
  url="https://github.com/buildkite/test-engine-client/releases/download/v${BKTEC_VERSION}/${asset}"

  curl -fsSL -o "${target}.tmp" "$url"
  chmod +x "${target}.tmp"
  mv "${target}.tmp" "$target"
  ln -sf "$target" "${bin_dir}/bktec"
}

install_bktec

export PATH="${bin_dir}:$PATH"
export BUILDKITE_TEST_ENGINE_TEST_RUNNER="${BUILDKITE_TEST_ENGINE_TEST_RUNNER:-gotest}"
export BUILDKITE_TEST_ENGINE_RETRY_COUNT="${BUILDKITE_TEST_ENGINE_RETRY_COUNT:-1}"
export BUILDKITE_TEST_ENGINE_RESULT_PATH="tmp/test-engine/gotest-${BUILDKITE_PARALLEL_JOB:-0}.xml"

if [[ -z "${BUILDKITE_TEST_ENGINE_SUITE_SLUG:-}" ]]; then
  echo "BUILDKITE_TEST_ENGINE_SUITE_SLUG is required" >&2
  exit 64
fi

if ! command -v gotestsum >/dev/null 2>&1; then
  echo "gotestsum is required; install it with mise before running Test Engine" >&2
  exit 127
fi

bktec run
