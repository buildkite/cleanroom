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
upload_smoke_artifacts() {
  local status="$1"
  if [[ -z "${BUILDKITE:-}" ]]; then
    return 0
  fi
  if ! command -v buildkite-agent >/dev/null 2>&1; then
    return 0
  fi
  if [[ ! -d "$tmpdir" ]]; then
    return 0
  fi

  local artifact_path
  artifact_path="$(mktemp "/tmp/cleanroom-ci-example-smoke-${BACKEND}.XXXXXX.tgz")"
  if tar -czf "$artifact_path" -C "$tmpdir" .; then
    echo "--- :package: Upload example smoke artifacts ($BACKEND, status=$status)"
    buildkite-agent artifact upload "$artifact_path" || true
  fi
  rm -f "$artifact_path"
}

cleanup() {
  local status="$1"
  if [[ -n "${wildcard_pid:-}" ]]; then
    kill "$wildcard_pid" >/dev/null 2>&1 || true
    wait "$wildcard_pid" >/dev/null 2>&1 || true
  fi
  upload_smoke_artifacts "$status" || true
  rm -rf "$tmpdir"
}
trap 'status=$?; cleanup "$status"; exit "$status"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

basic_smoke_dir="$tmpdir/basic"
docker_smoke_dir="$tmpdir/docker"
wildcard_example_dir="$REPO_ROOT/examples/wildcard-routing"
exposure_cert_path="${XDG_CONFIG_HOME:-$HOME/.config}/cleanroom/tls/exposure-cert.pem"
curl_max_time_seconds="${CLEANROOM_CI_CURL_MAX_TIME_SECONDS:-15}"
curl_connect_timeout_seconds="${CLEANROOM_CI_CURL_CONNECT_TIMEOUT_SECONDS:-5}"
wildcard_debug_log="$tmpdir/wildcard-debug.log"

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
  "$REPO_ROOT/examples/seeded-output-cache" \
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

echo "--- :whale: Docker cache-output regression smoke test ($BACKEND)"
# shellcheck disable=SC2016
"$REPO_ROOT/dist/cleanroom" exec --host "$LISTEN_ENDPOINT" --backend "$BACKEND" -c "$REPO_ROOT/examples/docker-cache-output" -- sh -lc 'docker image inspect "$1" >/dev/null && echo docker-cached-ok' sh "$docker_pull_image" | tee "$tmpdir/docker-cached.out"
if ! grep -q '^docker-cached-ok$' "$tmpdir/docker-cached.out"; then
  echo "expected docker cache-output regression smoke output missing" >&2
  exit 1
fi

echo "--- :package: Seeded cache-output smoke test ($BACKEND)"
"$REPO_ROOT/dist/cleanroom" exec --host "$LISTEN_ENDPOINT" --backend "$BACKEND" -c "$REPO_ROOT/examples/seeded-output-cache" -- sh -lc 'test -f examples/seeded-output-cache/public/assets/.keep && grep -q "^generated$" examples/seeded-output-cache/public/assets/generated.txt && echo seeded-cache-output-ok' | tee "$tmpdir/seeded-cache-output.out"
if ! grep -q '^seeded-cache-output-ok$' "$tmpdir/seeded-cache-output.out"; then
  echo "expected seeded cache-output smoke output missing" >&2
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
{
  echo "backend=$BACKEND"
  echo "listen_endpoint=$LISTEN_ENDPOINT"
  echo "wildcard_example_dir=$wildcard_example_dir"
  echo "curl_max_time_seconds=$curl_max_time_seconds"
  echo "curl_connect_timeout_seconds=$curl_connect_timeout_seconds"
  date -u '+started_at=%Y-%m-%dT%H:%M:%SZ'
} >>"$wildcard_debug_log"
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
echo "wildcard exposure ready: $wildcard_url" >>"$wildcard_debug_log"

wildcard_port="${wildcard_url##*:}"
if [[ ! -f "$exposure_cert_path" ]]; then
  echo "expected exposure certificate at $exposure_cert_path" >&2
  cat "$tmpdir/wildcard.stdout" >&2 || true
  cat "$tmpdir/wildcard.stderr" >&2 || true
  exit 1
fi
echo "wildcard_port=$wildcard_port" >>"$wildcard_debug_log"
echo "exposure_cert_path=$exposure_cert_path" >>"$wildcard_debug_log"

curl_retry() {
  local label="$1"
  local output_file="$2"
  shift 2
  local attempt
  local curl_status
  for attempt in $(seq 1 60); do
    echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') curl $label attempt $attempt/60" >>"$wildcard_debug_log"
    if curl --silent --show-error --fail-with-body \
      --connect-timeout "$curl_connect_timeout_seconds" \
      --max-time "$curl_max_time_seconds" \
      --cacert "$exposure_cert_path" "$@" >"$output_file"; then
      echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') curl $label attempt $attempt/60 succeeded" >>"$wildcard_debug_log"
      return 0
    fi
    curl_status=$?
    echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') curl $label attempt $attempt/60 failed status=$curl_status" >>"$wildcard_debug_log"
    if [[ -s "$output_file" ]]; then
      echo "--- $label response tail ---" >>"$wildcard_debug_log"
      tail -40 "$output_file" >>"$wildcard_debug_log" || true
      echo "--- end $label response tail ---" >>"$wildcard_debug_log"
    fi
    sleep 1
  done
  return 1
}

curl_headers_retry() {
  local label="$1"
  local output_file="$2"
  shift 2
  local body_file="${output_file}.body"
  local attempt
  local curl_status
  for attempt in $(seq 1 60); do
    echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') curl $label headers attempt $attempt/60" >>"$wildcard_debug_log"
    if curl --silent --show-error --fail-with-body \
      --connect-timeout "$curl_connect_timeout_seconds" \
      --max-time "$curl_max_time_seconds" \
      --cacert "$exposure_cert_path" \
      --dump-header "$output_file" \
      --output "$body_file" "$@"; then
      echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') curl $label headers attempt $attempt/60 succeeded" >>"$wildcard_debug_log"
      return 0
    fi
    curl_status=$?
    echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') curl $label headers attempt $attempt/60 failed status=$curl_status" >>"$wildcard_debug_log"
    if [[ -s "$output_file" ]]; then
      echo "--- $label headers tail ---" >>"$wildcard_debug_log"
      tail -40 "$output_file" >>"$wildcard_debug_log" || true
      echo "--- end $label headers tail ---" >>"$wildcard_debug_log"
    fi
    if [[ -s "$body_file" ]]; then
      echo "--- $label body tail ---" >>"$wildcard_debug_log"
      tail -40 "$body_file" >>"$wildcard_debug_log" || true
      echo "--- end $label body tail ---" >>"$wildcard_debug_log"
    fi
    sleep 1
  done
  return 1
}

if ! curl_retry "wildcard-exact" "$tmpdir/wildcard-exact.out" \
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

if ! curl_retry "wildcard-app" "$tmpdir/wildcard-app.out" \
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

if ! curl_headers_retry "wildcard-redirect" "$tmpdir/wildcard-redirect.headers" \
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
if ! grep -qi "^Location: https://app.example.cleanroom.localhost:${wildcard_port}/from-s3?client=127.0.0.1"$'\r' "$tmpdir/wildcard-redirect.headers"; then
  echo "expected wildcard redirect location missing" >&2
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
