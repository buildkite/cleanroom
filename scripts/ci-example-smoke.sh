#!/usr/bin/env bash
set -euo pipefail

BACKEND="${1:-}"
LISTEN_ENDPOINT="${2:-}"
REPO_ROOT="${3:-}"

if [[ -z "$BACKEND" || -z "$LISTEN_ENDPOINT" || -z "$REPO_ROOT" ]]; then
  echo "usage: $0 <backend> <listen-endpoint> <repo-root>" >&2
  exit 1
fi

tmpdir="$(mktemp -d)"
cleanup() {
  rm -rf "$tmpdir"
}
trap cleanup EXIT

for example_dir in \
  "$REPO_ROOT/examples/basic" \
  "$REPO_ROOT/examples/docker" \
  "$REPO_ROOT/examples/rails" \
  "$REPO_ROOT/examples/buildkite-agent"; do
  echo "--- :mag: Validate $(basename "$example_dir") example"
  (
    cd "$example_dir"
    "$REPO_ROOT/dist/cleanroom" policy validate
  )
done

echo "--- :white_check_mark: Basic example smoke test ($BACKEND)"
"$REPO_ROOT/dist/cleanroom" exec --host "$LISTEN_ENDPOINT" --backend "$BACKEND" -c "$REPO_ROOT/examples/basic" -- sh -lc 'echo basic-example-ok' | tee "$tmpdir/basic.out"
if ! grep -q '^basic-example-ok$' "$tmpdir/basic.out"; then
  echo "expected basic example output missing" >&2
  exit 1
fi

echo "--- :whale: Docker example version smoke test ($BACKEND)"
"$REPO_ROOT/dist/cleanroom" exec --host "$LISTEN_ENDPOINT" --backend "$BACKEND" -c "$REPO_ROOT/examples/docker" -- sh -lc 'docker version >/dev/null && echo docker-version-ok' | tee "$tmpdir/docker-version.out"
if ! grep -q '^docker-version-ok$' "$tmpdir/docker-version.out"; then
  echo "expected docker version smoke output missing" >&2
  exit 1
fi

echo "--- :whale: Docker example pull smoke test ($BACKEND)"
"$REPO_ROOT/dist/cleanroom" exec --host "$LISTEN_ENDPOINT" --backend "$BACKEND" -c "$REPO_ROOT/examples/docker" -- sh -lc 'docker pull alpine:3.22 >/dev/null && echo docker-pull-ok' | tee "$tmpdir/docker-pull.out"
if ! grep -q '^docker-pull-ok$' "$tmpdir/docker-pull.out"; then
  echo "expected docker pull smoke output missing" >&2
  exit 1
fi

if [[ "$BACKEND" = "firecracker" ]]; then
  echo "--- :whale: Docker example run smoke test ($BACKEND skipped)"
  echo "skipping docker run smoke on firecracker: guest docker pull path is covered, but guest container start is not yet reliable on this backend"
  exit 0
fi

echo "--- :whale: Docker example run smoke test ($BACKEND)"
"$REPO_ROOT/dist/cleanroom" exec --host "$LISTEN_ENDPOINT" --backend "$BACKEND" -c "$REPO_ROOT/examples/docker" -- docker run --rm --network none alpine:3.22 echo docker-example-ok | tee "$tmpdir/docker-run.out"
if ! grep -q '^docker-example-ok$' "$tmpdir/docker-run.out"; then
  echo "expected docker example output missing" >&2
  exit 1
fi
