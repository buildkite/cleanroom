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
  if [[ -n "${wildcard_pid:-}" ]]; then
    kill "$wildcard_pid" >/dev/null 2>&1 || true
    wait "$wildcard_pid" >/dev/null 2>&1 || true
  fi
  rm -rf "$tmpdir"
}
trap cleanup EXIT

basic_smoke_dir="$tmpdir/basic"
docker_smoke_dir="$tmpdir/docker"
wildcard_example_dir="$REPO_ROOT/examples/wildcard-routing"
exposure_cert_path="${XDG_CONFIG_HOME:-$HOME/.config}/cleanroom/tls/exposure-cert.pem"

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
  "$REPO_ROOT/examples/rails" \
  "$REPO_ROOT/examples/buildkite-agent" \
  "$REPO_ROOT/examples/wildcard-routing"; do
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

echo "--- :globe_with_meridians: Wildcard routing example smoke test ($BACKEND)"
"$REPO_ROOT/dist/cleanroom" exec \
  --host "$LISTEN_ENDPOINT" \
  --backend "$BACKEND" \
  --no-stdin \
  -c "$wildcard_example_dir" \
  --expose-https example:80 \
  --expose-https '*.example:80' \
  -- sh -lc 'cd /workspace/examples/wildcard-routing && sh ./start.sh' >"$tmpdir/wildcard.stdout" 2>"$tmpdir/wildcard.stderr" &
wildcard_pid=$!

wildcard_url=""
for _ in $(seq 1 120); do
  if ! kill -0 "$wildcard_pid" >/dev/null 2>&1; then
    echo "wildcard example exited before exposure was ready" >&2
    cat "$tmpdir/wildcard.stdout" >&2 || true
    cat "$tmpdir/wildcard.stderr" >&2 || true
    wait "$wildcard_pid"
    exit 1
  fi
  wildcard_url="$(grep -m1 '^exposed: https://example\.cleanroom\.localhost:' "$tmpdir/wildcard.stderr" | sed 's/^exposed: //' || true)"
  if [[ -n "$wildcard_url" ]]; then
    break
  fi
  sleep 1
done
if [[ -z "$wildcard_url" ]]; then
  echo "timed out waiting for wildcard example exposure" >&2
  cat "$tmpdir/wildcard.stdout" >&2 || true
  cat "$tmpdir/wildcard.stderr" >&2 || true
  exit 1
fi

wildcard_port="${wildcard_url##*:}"
if [[ ! -f "$exposure_cert_path" ]]; then
  echo "expected exposure certificate at $exposure_cert_path" >&2
  cat "$tmpdir/wildcard.stdout" >&2 || true
  cat "$tmpdir/wildcard.stderr" >&2 || true
  exit 1
fi

curl_retry() {
  local output_file="$1"
  shift
  for _ in $(seq 1 60); do
    if curl --silent --show-error --fail-with-body --cacert "$exposure_cert_path" "$@" >"$output_file"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

curl_headers_retry() {
  local output_file="$1"
  shift
  for _ in $(seq 1 60); do
    if curl --silent --show-error --fail-with-body --cacert "$exposure_cert_path" --dump-header "$output_file" --output /dev/null "$@"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

if ! curl_retry "$tmpdir/wildcard-exact.out" \
  --resolve "example.cleanroom.localhost:${wildcard_port}:127.0.0.1" \
  "https://example.cleanroom.localhost:${wildcard_port}/"; then
  echo "wildcard exact route did not become ready" >&2
  cat "$tmpdir/wildcard.stdout" >&2 || true
  cat "$tmpdir/wildcard.stderr" >&2 || true
  exit 1
fi
if ! grep -q '^exact route ok$' "$tmpdir/wildcard-exact.out"; then
  echo "expected exact wildcard route output missing" >&2
  cat "$tmpdir/wildcard-exact.out" >&2 || true
  exit 1
fi

if ! curl_retry "$tmpdir/wildcard-app.out" \
  --resolve "app.example.cleanroom.localhost:${wildcard_port}:127.0.0.1" \
  "https://app.example.cleanroom.localhost:${wildcard_port}/"; then
  echo "wildcard app route did not become ready" >&2
  cat "$tmpdir/wildcard.stdout" >&2 || true
  cat "$tmpdir/wildcard.stderr" >&2 || true
  exit 1
fi
for needle in \
  '"host": "app.example.cleanroom.localhost:'"$wildcard_port"'"' \
  '"x_forwarded_host": "app.example.cleanroom.localhost:'"$wildcard_port"'"' \
  '"x_forwarded_proto": "https"' \
  '"x_forwarded_port": "'"$wildcard_port"'"' \
  '"x_forwarded_for": "127.0.0.1"'; do
  if ! grep -q "$needle" "$tmpdir/wildcard-app.out"; then
    echo "expected wildcard app response to contain $needle" >&2
    cat "$tmpdir/wildcard-app.out" >&2 || true
    exit 1
  fi
done

if ! curl_headers_retry "$tmpdir/wildcard-redirect.headers" \
  --resolve "s3.example.cleanroom.localhost:${wildcard_port}:127.0.0.1" \
  "https://s3.example.cleanroom.localhost:${wildcard_port}/"; then
  echo "wildcard redirect route did not become ready" >&2
  cat "$tmpdir/wildcard.stdout" >&2 || true
  cat "$tmpdir/wildcard.stderr" >&2 || true
  exit 1
fi
if ! grep -q '^HTTP/.* 302' "$tmpdir/wildcard-redirect.headers"; then
  echo "expected wildcard redirect status missing" >&2
  cat "$tmpdir/wildcard-redirect.headers" >&2 || true
  exit 1
fi
if ! grep -q "^Location: https://app.example.cleanroom.localhost:${wildcard_port}/from-s3?client=127.0.0.1"$'\r' "$tmpdir/wildcard-redirect.headers"; then
  echo "expected wildcard redirect location missing" >&2
  cat "$tmpdir/wildcard-redirect.headers" >&2 || true
  exit 1
fi

wildcard_deep_status="$(curl --silent --show-error \
  --cacert "$exposure_cert_path" \
  --resolve "foo.bar.example.cleanroom.localhost:${wildcard_port}:127.0.0.1" \
  --output "$tmpdir/wildcard-deep.out" \
  --write-out '%{http_code}' \
  "https://foo.bar.example.cleanroom.localhost:${wildcard_port}/")"
if [[ "$wildcard_deep_status" != "404" ]]; then
  echo "expected deeper wildcard host to return 404, got $wildcard_deep_status" >&2
  cat "$tmpdir/wildcard-deep.out" >&2 || true
  exit 1
fi
if ! grep -q '^404 page not found$' "$tmpdir/wildcard-deep.out"; then
  echo "expected deeper wildcard 404 body missing" >&2
  cat "$tmpdir/wildcard-deep.out" >&2 || true
  exit 1
fi

kill "$wildcard_pid" >/dev/null 2>&1 || true
wait "$wildcard_pid" >/dev/null 2>&1 || true
wildcard_pid=""

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
