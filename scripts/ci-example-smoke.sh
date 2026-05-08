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

basic_smoke_dir="$tmpdir/basic"
docker_smoke_dir="$tmpdir/docker"

mkdir -p "$basic_smoke_dir" "$docker_smoke_dir"
cat > "$basic_smoke_dir/cleanroom.yaml" <<'EOF'
version: 1
repository:
  enabled: false
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:91a63856cdf97b2e5659660b41d1a131d3b57bfa4cad254018e391ffef6fa4b9
  network:
    default: deny
    allow:
      - host: api.github.com
        ports: [443]
EOF
cat > "$docker_smoke_dir/cleanroom.yaml" <<'EOF'
version: 1
repository:
  enabled: false
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine-docker@sha256:19c696770ae8f3f36e786bf25a0e08e5a5c18b9a7fe52bde7d988c3da500bf08
  docker:
    required: true
  network:
    default: deny
    allow:
      - host: ghcr.io
        ports: [443]
      - host: pkg-containers.githubusercontent.com
        ports: [443]
EOF
docker_pull_image="ghcr.io/buildkite/cleanroom-base/alpine@sha256:91a63856cdf97b2e5659660b41d1a131d3b57bfa4cad254018e391ffef6fa4b9"

for example_dir in \
  "$REPO_ROOT/examples/basic" \
  "$REPO_ROOT/examples/docker" \
  "$REPO_ROOT/examples/docker-cache-output" \
  "$REPO_ROOT/examples/rails" \
  "$REPO_ROOT/examples/buildkite-agent"; do
  echo "--- :mag: Validate $(basename "$example_dir") example"
  (
    cd "$example_dir"
    "$REPO_ROOT/dist/cleanroom" policy validate
  )
done

echo "--- :white_check_mark: Basic example smoke test ($BACKEND)"
"$REPO_ROOT/dist/cleanroom" exec --host "$LISTEN_ENDPOINT" --backend "$BACKEND" -c "$basic_smoke_dir" -- sh -lc 'echo basic-example-ok' | tee "$tmpdir/basic.out"
if ! grep -q '^basic-example-ok$' "$tmpdir/basic.out"; then
  echo "expected basic example output missing" >&2
  exit 1
fi

echo "--- :whale: Docker example version smoke test ($BACKEND)"
"$REPO_ROOT/dist/cleanroom" exec --host "$LISTEN_ENDPOINT" --backend "$BACKEND" -c "$docker_smoke_dir" -- sh -lc 'docker version >/dev/null && echo docker-version-ok' | tee "$tmpdir/docker-version.out"
if ! grep -q '^docker-version-ok$' "$tmpdir/docker-version.out"; then
  echo "expected docker version smoke output missing" >&2
  exit 1
fi

echo "--- :whale: Docker cache-output regression smoke test ($BACKEND)"
"$REPO_ROOT/dist/cleanroom" exec --host "$LISTEN_ENDPOINT" --backend "$BACKEND" -c "$REPO_ROOT/examples/docker-cache-output" -- sh -lc 'docker version >/dev/null && echo docker-cached-ok' | tee "$tmpdir/docker-cached.out"
if ! grep -q '^docker-cached-ok$' "$tmpdir/docker-cached.out"; then
  echo "expected docker cache-output regression smoke output missing" >&2
  exit 1
fi

echo "--- :whale: Docker example pull smoke test ($BACKEND)"
pull_attempt=1
pull_max_attempts=3
while true; do
  set +e
  # shellcheck disable=SC2016
  "$REPO_ROOT/dist/cleanroom" exec --host "$LISTEN_ENDPOINT" --backend "$BACKEND" -c "$docker_smoke_dir" -- sh -lc 'docker pull "$1" >/dev/null && echo docker-pull-ok' sh "$docker_pull_image" | tee "$tmpdir/docker-pull.out"
  pull_status=${PIPESTATUS[0]}
  set -e

  if [[ "$pull_status" -eq 0 ]]; then
    break
  fi
  if [[ "$pull_attempt" -lt "$pull_max_attempts" ]]; then
    echo "docker pull smoke failed on attempt $pull_attempt/$pull_max_attempts; retrying"
    sleep "$pull_attempt"
    pull_attempt=$((pull_attempt + 1))
    continue
  fi
  echo "docker pull smoke failed after $pull_max_attempts attempts" >&2
  exit "$pull_status"
done
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
"$REPO_ROOT/dist/cleanroom" exec --host "$LISTEN_ENDPOINT" --backend "$BACKEND" -c "$docker_smoke_dir" -- docker run --rm --network none "$docker_pull_image" echo docker-example-ok | tee "$tmpdir/docker-run.out"
if ! grep -q '^docker-example-ok$' "$tmpdir/docker-run.out"; then
  echo "expected docker example output missing" >&2
  exit 1
fi
