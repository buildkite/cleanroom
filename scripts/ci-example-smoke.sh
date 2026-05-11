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
  if [[ -n "${multi_host_pid:-}" ]]; then
    kill "$multi_host_pid" >/dev/null 2>&1 || true
    wait "$multi_host_pid" >/dev/null 2>&1 || true
  fi
  rm -rf "$tmpdir"
}
trap cleanup EXIT

basic_smoke_dir="$tmpdir/basic"
docker_smoke_dir="$tmpdir/docker"
multi_host_example_dir="$REPO_ROOT/examples/multi-host-routing"
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
  "$REPO_ROOT/examples/docker-cache-output" \
  "$REPO_ROOT/examples/seeded-output-cache" \
  "$REPO_ROOT/examples/rails" \
  "$REPO_ROOT/examples/buildkite-agent" \
  "$REPO_ROOT/examples/multi-host-routing"; do
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

echo "--- :globe_with_meridians: Multi-host routing example smoke test ($BACKEND)"
"$REPO_ROOT/dist/cleanroom" exec \
  --host "$LISTEN_ENDPOINT" \
  --backend "$BACKEND" \
  --no-stdin \
  -c "$multi_host_example_dir" \
  --expose-https example:80 \
  --expose-https example-app:80 \
  --expose-https example-s3:80 \
  -- sh -lc 'cd /workspace/examples/multi-host-routing && sh ./start.sh' >"$tmpdir/multi-host.stdout" 2>"$tmpdir/multi-host.stderr" &
multi_host_pid=$!

multi_host_url=""
for _ in $(seq 1 120); do
  if ! kill -0 "$multi_host_pid" >/dev/null 2>&1; then
    echo "multi-host example exited before exposure was ready" >&2
    cat "$tmpdir/multi-host.stdout" >&2 || true
    cat "$tmpdir/multi-host.stderr" >&2 || true
    wait "$multi_host_pid"
    exit 1
  fi
  multi_host_url="$(grep -m1 '^exposed: https://example\.cleanroom\.localhost:' "$tmpdir/multi-host.stderr" | sed 's/^exposed: //' || true)"
  if [[ -n "$multi_host_url" ]]; then
    break
  fi
  sleep 1
done
if [[ -z "$multi_host_url" ]]; then
  echo "timed out waiting for multi-host example exposure" >&2
  cat "$tmpdir/multi-host.stdout" >&2 || true
  cat "$tmpdir/multi-host.stderr" >&2 || true
  exit 1
fi

multi_host_port="${multi_host_url##*:}"
if [[ ! -f "$exposure_cert_path" ]]; then
  echo "expected exposure certificate at $exposure_cert_path" >&2
  exit 1
fi

curl_retry() {
  local label="$1"
  local output_file="$2"
  shift 2
  for _ in $(seq 1 60); do
    if curl --silent --show-error --fail-with-body \
      --connect-timeout 5 \
      --max-time 15 \
      --cacert "$exposure_cert_path" "$@" >"$output_file"; then
      return 0
    fi
    sleep 1
  done
  echo "$label did not become ready" >&2
  cat "$tmpdir/multi-host.stdout" >&2 || true
  cat "$tmpdir/multi-host.stderr" >&2 || true
  return 1
}

curl_headers_retry() {
  local label="$1"
  local output_file="$2"
  shift 2
  local body_file="${output_file}.body"
  for _ in $(seq 1 60); do
    if curl --silent --show-error --fail-with-body \
      --connect-timeout 5 \
      --max-time 15 \
      --cacert "$exposure_cert_path" \
      --dump-header "$output_file" \
      --output "$body_file" "$@"; then
      return 0
    fi
    sleep 1
  done
  echo "$label did not become ready" >&2
  cat "$tmpdir/multi-host.stdout" >&2 || true
  cat "$tmpdir/multi-host.stderr" >&2 || true
  return 1
}

curl_status_retry() {
  local label="$1"
  local output_file="$2"
  local expected_status="$3"
  shift 3
  local status_file="${output_file}.status"
  for _ in $(seq 1 60); do
    if curl --silent --show-error \
      --connect-timeout 5 \
      --max-time 15 \
      --cacert "$exposure_cert_path" \
      --write-out '%{http_code}' \
      --output "$output_file" "$@" >"$status_file" && [[ "$(cat "$status_file")" == "$expected_status" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "$label did not return HTTP $expected_status" >&2
  if [[ -f "$status_file" ]]; then
    echo "last status: $(cat "$status_file")" >&2
  fi
  cat "$tmpdir/multi-host.stdout" >&2 || true
  cat "$tmpdir/multi-host.stderr" >&2 || true
  return 1
}

curl_retry "multi-host exact route" "$tmpdir/multi-host-exact.out" \
  --resolve "example.cleanroom.localhost:${multi_host_port}:127.0.0.1" \
  "https://example.cleanroom.localhost:${multi_host_port}/"
if ! grep -q '^exact route ok$' "$tmpdir/multi-host-exact.out"; then
  echo "expected multi-host exact route output missing" >&2
  cat "$tmpdir/multi-host-exact.out" >&2 || true
  exit 1
fi

curl_retry "multi-host app route" "$tmpdir/multi-host-app.out" \
  --resolve "example-app.cleanroom.localhost:${multi_host_port}:127.0.0.1" \
  "https://example-app.cleanroom.localhost:${multi_host_port}/"
for needle in \
  '"host": "example-app.cleanroom.localhost:'"$multi_host_port"'"' \
  '"x_forwarded_host": "example-app.cleanroom.localhost:'"$multi_host_port"'"' \
  '"x_forwarded_proto": "https"' \
  '"x_forwarded_port": "'"$multi_host_port"'"' \
  '"x_forwarded_for": "127.0.0.1"'; do
  if ! grep -Fq "$needle" "$tmpdir/multi-host-app.out"; then
    echo "expected multi-host app response to contain $needle" >&2
    cat "$tmpdir/multi-host-app.out" >&2 || true
    exit 1
  fi
done

curl_headers_retry "multi-host redirect route" "$tmpdir/multi-host-redirect.headers" \
  --resolve "example-s3.cleanroom.localhost:${multi_host_port}:127.0.0.1" \
  "https://example-s3.cleanroom.localhost:${multi_host_port}/"
if ! grep -q '^HTTP/.* 302' "$tmpdir/multi-host-redirect.headers"; then
  echo "expected multi-host redirect status missing" >&2
  cat "$tmpdir/multi-host-redirect.headers" >&2 || true
  exit 1
fi
if ! tr -d '\r' <"$tmpdir/multi-host-redirect.headers" | grep -Fixq "Location: https://example-app.cleanroom.localhost:${multi_host_port}/from-s3?client=127.0.0.1"; then
  echo "expected multi-host redirect location missing" >&2
  cat "$tmpdir/multi-host-redirect.headers" >&2 || true
  exit 1
fi

curl_status_retry "multi-host unregistered route" "$tmpdir/multi-host-missing.out" "404" \
  --resolve "example-missing.cleanroom.localhost:${multi_host_port}:127.0.0.1" \
  "https://example-missing.cleanroom.localhost:${multi_host_port}/"
if ! grep -q '^404 page not found$' "$tmpdir/multi-host-missing.out"; then
  echo "expected multi-host missing route body missing" >&2
  cat "$tmpdir/multi-host-missing.out" >&2 || true
  exit 1
fi

kill "$multi_host_pid" >/dev/null 2>&1 || true
wait "$multi_host_pid" >/dev/null 2>&1 || true
multi_host_pid=""

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
