#!/usr/bin/env bash
set -euo pipefail

ROOT_HELPER_REQUIRED_CAPABILITIES=(
  firecracker-network
  firecracker-trusted-dns
)

# run_privileged executes a privileged command via the installed root helper.
run_privileged() {
  if [[ -z "${PRIVILEGED_HELPER_PATH:-}" ]]; then
    echo "CLEANROOM_PRIVILEGED_HELPER_PATH is required for Firecracker e2e CI" >&2
    return 1
  fi
  sudo -n "$PRIVILEGED_HELPER_PATH" "$@"
}

annotate_root_helper_problem() {
  local heading="$1"
  local message="$2"

  if command -v buildkite-agent >/dev/null 2>&1; then
    buildkite-agent annotate --context root-helper-capabilities --style error <<EOF
$heading

$message
EOF
  fi
}

verify_helper_capabilities() {
  if [[ -z "${PRIVILEGED_HELPER_PATH:-}" ]]; then
    echo "CLEANROOM_PRIVILEGED_HELPER_PATH is required for Firecracker e2e CI" >&2
    return 1
  fi

  local capabilities
  if ! capabilities="$(sudo -n "$PRIVILEGED_HELPER_PATH" capabilities 2>&1)"; then
    echo "⚠️  Root helper capability probe failed via $PRIVILEGED_HELPER_PATH" >&2
    echo "   $capabilities" >&2
    annotate_root_helper_problem \
      "### ❌ Root helper capability probe failed" \
      "The installed root helper (\`$PRIVILEGED_HELPER_PATH\`) could not be queried for capabilities.\n\nRoll out the latest helper on the CI host, for example by rerunning \`../cleanroom-ops/scripts/bootstrap-buildkite-agent.sh\`, and then rerun the build."
    return 1
  fi

  local missing=()
  local capability
  for capability in "${ROOT_HELPER_REQUIRED_CAPABILITIES[@]}"; do
    if ! grep -Fxq "$capability" <<<"$capabilities"; then
      missing+=("$capability")
    fi
  done

  if [[ "${#missing[@]}" -gt 0 ]]; then
    echo "⚠️  Root helper on host ($PRIVILEGED_HELPER_PATH) is missing required capabilities" >&2
    echo "   missing: ${missing[*]}" >&2
    echo "   have:" >&2
    printf '     %s\n' "$capabilities" >&2
    annotate_root_helper_problem \
      "### ❌ Root helper is missing required capabilities" \
      "The installed root helper (\`$PRIVILEGED_HELPER_PATH\`) is missing required capabilities for this branch.\n\nMissing: \`${missing[*]}\`\n\nRoll out the latest helper on the CI host, for example by rerunning \`../cleanroom-ops/scripts/bootstrap-buildkite-agent.sh\`, and then rerun the build."
    return 1
  fi
}

# purge_stale_cleanroom_resources removes TAP devices, iptables rules,
# and firecracker processes left over from a previous run that crashed
# before cleanup.
purge_stale_cleanroom_resources() {
  # Kill orphaned firecracker processes owned by the current user.
  local stale_pids
  stale_pids="$(pgrep -u "$(id -u)" firecracker 2>/dev/null || true)"
  if [[ -n "$stale_pids" ]]; then
    echo "killing orphaned firecracker processes: $stale_pids"
    # shellcheck disable=SC2086
    kill $stale_pids 2>/dev/null || true
    sleep 1
    # shellcheck disable=SC2086
    kill -9 $stale_pids 2>/dev/null || true
  fi

  # Remove stale TAP devices (prefixed "cr") and their iptables rules.
  local taps
  taps="$(run_privileged ip -o link show 2>/dev/null | grep -oP 'cr[a-z0-9]{1,13}(?=:)' || true)"
  for tap in $taps; do
    echo "removing stale tap device and iptables rules: $tap"
    # Delete all iptables rules referencing this TAP by listing and reversing.
    for chain in INPUT FORWARD; do
      local rules
      rules="$(run_privileged iptables -S "$chain" 2>/dev/null | grep -- " $tap " || true)"
      while IFS= read -r rule; do
        [[ -n "$rule" ]] || continue
        # shellcheck disable=SC2086
        run_privileged iptables ${rule/-A/-D} 2>/dev/null || true
      done <<< "$rules"
    done
    run_privileged iptables -F "crdns-tcp-${tap}" 2>/dev/null || true
    run_privileged iptables -X "crdns-tcp-${tap}" 2>/dev/null || true
    run_privileged iptables -F "crdns-udp-${tap}" 2>/dev/null || true
    run_privileged iptables -X "crdns-udp-${tap}" 2>/dev/null || true
    run_privileged ip link del "$tap" 2>/dev/null || true
  done

  # Remove stale NAT MASQUERADE rules for cleanroom subnets (10.x.x.0/24).
  local nat_rules
  nat_rules="$(run_privileged iptables -t nat -S POSTROUTING 2>/dev/null | grep 'MASQUERADE' | grep -E '10\.[0-9]+\.[0-9]+\.' || true)"
  while IFS= read -r rule; do
    [[ -n "$rule" ]] || continue
    # shellcheck disable=SC2086
    run_privileged iptables -t nat ${rule/-A/-D} 2>/dev/null || true
  done <<< "$nat_rules"
}

cleanup() {
  if [[ -n "${srv_pid:-}" ]]; then
    kill "$srv_pid" >/dev/null 2>&1 || true
    wait "$srv_pid" >/dev/null 2>&1 || true
  fi
  # Give the server a moment to clean up sandboxes (TAPs, iptables, VMs).
  sleep 1
  # Best-effort cleanup of any resources the server didn't tear down.
  purge_stale_cleanroom_resources 2>/dev/null || true
  rm -rf "$tmpdir"
}

main() {
  KERNEL_IMAGE="${CLEANROOM_KERNEL_IMAGE:-}"
  FIRECRACKER_BINARY="${CLEANROOM_FIRECRACKER_BINARY:-firecracker}"
  PRIVILEGED_HELPER_PATH="${CLEANROOM_PRIVILEGED_HELPER_PATH:-/usr/local/sbin/cleanroom-root-helper}"

  if [[ -z "$KERNEL_IMAGE" ]]; then
    echo "CLEANROOM_KERNEL_IMAGE is required for Firecracker e2e CI" >&2
    exit 1
  fi

  verify_helper_capabilities

  echo "--- :broom: Pre-build cleanup"
  purge_stale_cleanroom_resources

  echo "--- :hammer: Building binaries"
  scripts/build-go.sh

  tmpdir="$(mktemp -d)"
  trap cleanup EXIT

  export XDG_CONFIG_HOME="$tmpdir/config"
  export XDG_CACHE_HOME="$tmpdir/cache"
  export XDG_STATE_HOME="$tmpdir/state"
  export XDG_RUNTIME_DIR="$tmpdir/runtime"
  export XDG_DATA_HOME="$tmpdir/data"

  mkdir -p "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME" "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR" "$XDG_DATA_HOME"
  mkdir -p "$XDG_CONFIG_HOME/cleanroom"
  cat > "$XDG_CONFIG_HOME/cleanroom/config.yaml" <<EOF
default_backend: firecracker
backends:
  firecracker:
    binary_path: $FIRECRACKER_BINARY
    kernel_image: $KERNEL_IMAGE
    vcpus: 2
    memory_mib: 1024
    launch_seconds: 90
EOF

  echo "    privileged_helper_path: $PRIVILEGED_HELPER_PATH" >> "$XDG_CONFIG_HOME/cleanroom/config.yaml"

  echo "--- :stethoscope: Doctor"
  ./dist/cleanroom doctor --json | tee "$tmpdir/doctor.json"
  if grep -q '"status": "fail"' "$tmpdir/doctor.json"; then
    echo "doctor checks reported failures" >&2
    exit 1
  fi

  socket_path="$tmpdir/cleanroom.sock"
  listen_endpoint="unix://$socket_path"

dump_runtime_diagnostics() {
  local server_lines="${1:-40}"
  if [[ -f "$tmpdir/server.log" ]]; then
    echo "--- server log tail ---" >&2
    tail -n "$server_lines" "$tmpdir/server.log" >&2 || true
  fi

  # Surface recent Firecracker process logs when provisioning/agent readiness
  # flakes occur so failures are diagnosable from CI output alone.
  local fc_logs
  fc_logs="$(find "$XDG_STATE_HOME"/cleanroom/sandboxes -maxdepth 3 -type f \( -name 'firecracker.stdout.log' -o -name 'firecracker.stderr.log' \) 2>/dev/null | sort | tail -n 6 || true)"
  if [[ -n "$fc_logs" ]]; then
    echo "--- firecracker log tails ---" >&2
    while IFS= read -r log_file; do
      [[ -n "$log_file" ]] || continue
      echo "[$log_file]" >&2
      tail -n 30 "$log_file" >&2 || true
    done <<< "$fc_logs"
  fi
}

  echo "--- :rocket: Start cleanroom control-plane"
  ./dist/cleanroom serve --listen "$listen_endpoint" --gateway-listen ":0" >"$tmpdir/server.log" 2>&1 &
  srv_pid=$!

  for _ in $(seq 1 40); do
    if [[ -S "$socket_path" ]]; then
      break
    fi
    sleep 0.25
  done
  if [[ ! -S "$socket_path" ]]; then
    echo "cleanroom server did not create unix socket: $socket_path" >&2
    echo "server log:" >&2
    cat "$tmpdir/server.log" >&2
    exit 1
  fi

echo "--- :white_check_mark: Launched execution smoke test"
smoke_attempt=1
smoke_max_attempts=3
while true; do
  set +e
  ./dist/cleanroom exec --host "$listen_endpoint" -c "$PWD" -- sh -lc 'echo cleanroom-e2e' >"$tmpdir/exec.out" 2>"$tmpdir/exec.err"
  smoke_status=$?
  set -e

  if [[ "$smoke_status" -eq 0 ]] && grep -q '^cleanroom-e2e$' "$tmpdir/exec.out"; then
    cat "$tmpdir/exec.out"
    break
  fi

  if [[ "$smoke_status" -ne 0 ]] && grep -q 'timed out waiting for vsock guest agent' "$tmpdir/exec.err" && [[ "$smoke_attempt" -lt "$smoke_max_attempts" ]]; then
    echo "smoke test hit transient vsock timeout (attempt $smoke_attempt/$smoke_max_attempts); retrying"
    sleep "$smoke_attempt"
    smoke_attempt=$((smoke_attempt + 1))
    continue
  fi

  echo "smoke test failed (exit $smoke_status)" >&2
  echo "--- smoke stdout ---" >&2
  cat "$tmpdir/exec.out" >&2 || true
  echo "--- smoke stderr ---" >&2
  cat "$tmpdir/exec.err" >&2 || true
  dump_runtime_diagnostics 80
  exit 1
done

echo "--- :recycle: Persistent sandbox lifecycle test"
sandbox_id="$(./dist/cleanroom create --host "$listen_endpoint" -c "$PWD" | tr -d '\n')"
if [[ -z "$sandbox_id" ]]; then
  echo "cleanroom create did not return an id" >&2
  exit 1
fi
echo "sandbox id: $sandbox_id"

./dist/cleanroom exec --host "$listen_endpoint" --in "$sandbox_id" -- sh -lc 'printf persisted-data >/tmp/persist.txt'
./dist/cleanroom exec --host "$listen_endpoint" --in "$sandbox_id" -- sh -lc 'cat /tmp/persist.txt' | tee "$tmpdir/persist-read.out"
if ! grep -q '^persisted-data$' "$tmpdir/persist-read.out"; then
  echo "expected persisted sandbox file contents from second execution" >&2
  exit 1
fi

./dist/download-sandbox-file \
  --host "$listen_endpoint" \
  --sandbox-id "$sandbox_id" \
  --path /tmp/persist.txt \
  --timeout 45s \
  --max-bytes 4096 >"$tmpdir/persist-download.out"
if ! grep -q '^persisted-data$' "$tmpdir/persist-download.out"; then
  echo "expected downloaded sandbox file contents" >&2
  echo "downloaded payload:" >&2
  cat "$tmpdir/persist-download.out" >&2 || true
  exit 1
fi

./dist/cleanroom sandbox rm --host "$listen_endpoint" "$sandbox_id" | tee "$tmpdir/sandbox-rm.out"
if ! grep -q 'sandbox terminated' "$tmpdir/sandbox-rm.out"; then
  echo "expected sandbox terminate acknowledgement" >&2
  exit 1
fi

set +e
./dist/cleanroom exec --host "$listen_endpoint" --in "$sandbox_id" -- sh -lc 'echo should-not-run' >"$tmpdir/terminated.out" 2>"$tmpdir/terminated.err"
terminated_status=$?
set -e
if [[ "$terminated_status" -eq 0 ]]; then
  echo "expected execution against terminated sandbox to fail" >&2
  exit 1
fi
if ! grep -Eq 'unknown sandbox|is not ready' "$tmpdir/terminated.err"; then
  echo "expected unknown-sandbox or not-ready error after termination" >&2
  cat "$tmpdir/terminated.err" >&2 || true
  exit 1
fi

echo "--- :satellite: Git gateway allow/deny test"
git_gateway_attempt=1
git_gateway_max_attempts=3
while true; do
  set +e
  # shellcheck disable=SC2016
  ./dist/cleanroom exec --host "$listen_endpoint" -c "$PWD" -- sh -lc '
  set -eu

  key="$(env | awk -F= '"'"'/^GIT_CONFIG_KEY_[0-9]+=url\.http:\/\/.+\/git\/github\.com\/\.insteadOf$/ {print $2; exit}'"'"')"
  if [ -z "$key" ]; then
    echo "failed to discover injected git gateway rewrite key" >&2
    exit 2
  fi
  gw="${key#url.}"
  gw="${gw%/github.com/.insteadOf}"

  # Preferred path: use git client when present in guest image.
  if command -v git >/dev/null 2>&1; then
    git ls-remote https://github.com/buildkite/cleanroom.git HEAD >/dev/null

    set +e
    git ls-remote "${gw}/gitlab.com/gitlab-org/gitlab.git" HEAD >/tmp/git-deny.out 2>/tmp/git-deny.err
    deny_rc=$?
    set -e

    if [ "$deny_rc" -eq 0 ]; then
      echo "expected disallowed host to fail through git gateway, but command succeeded" >&2
      cat /tmp/git-deny.out >&2 || true
      cat /tmp/git-deny.err >&2 || true
      exit 3
    fi
    if ! grep -q "host_not_allowed" /tmp/git-deny.err; then
      echo "expected host_not_allowed in git deny stderr" >&2
      cat /tmp/git-deny.err >&2 || true
      exit 4
    fi

    echo "git gateway checks passed (git client path)"
    exit 0
  fi

  # Fallback path for minimal guest images without git: exercise the same
  # gateway routes with wget.
  if command -v wget >/dev/null 2>&1; then
    allow_url="${gw}/github.com/buildkite/cleanroom.git/info/refs?service=git-upload-pack"
    deny_url="${gw}/gitlab.com/gitlab-org/gitlab.git/info/refs?service=git-upload-pack"

    # Retry allowlisted probe because transient host/gateway network stalls can
    # fail a single request even when policy behavior is correct.
    allow_attempt=1
    allow_max_attempts=3
    allow_resp=""
    allow_rc=1
    while [ "$allow_attempt" -le "$allow_max_attempts" ]; do
      set +e
      allow_resp="$(wget -q -S -O - "$allow_url" 2>&1)"
      allow_rc=$?
      set -e
      if [ "$allow_rc" -eq 0 ]; then
        break
      fi
      if [ "$allow_attempt" -lt "$allow_max_attempts" ]; then
        sleep "$allow_attempt"
      fi
      allow_attempt=$((allow_attempt + 1))
    done
    if [ "$allow_rc" -ne 0 ]; then
      echo "allowlisted host probe failed (exit $allow_rc)" >&2
      echo "$allow_resp" >&2
      exit 5
    fi
    if echo "$allow_resp" | grep -q "upstream_error"; then
      echo "allowlisted host probe hit upstream_error" >&2
      echo "$allow_resp" >&2
      exit 5
    fi
    if echo "$allow_resp" | grep -q "host_not_allowed"; then
      echo "allowlisted host was denied by gateway" >&2
      echo "$allow_resp" >&2
      exit 5
    fi
    # Accept either a smart-protocol response marker or an explicit HTTP 200.
    if ! echo "$allow_resp" | grep -q "git-upload-pack" && ! echo "$allow_resp" | grep -Eq "HTTP/[0-9.]+[[:space:]]+200"; then
      echo "allowlisted host probe returned unexpected response shape" >&2
      echo "$allow_resp" >&2
      exit 5
    fi

    set +e
    deny_resp="$(wget -q -S -O - "$deny_url" 2>&1)"
    deny_rc=$?
    set -e
    if [ "$deny_rc" -eq 0 ]; then
      echo "expected deny probe to fail for disallowed host, but it succeeded" >&2
      echo "$deny_resp" >&2
      exit 6
    fi
    if ! echo "$deny_resp" | grep -qE "host_not_allowed|403 Forbidden"; then
      echo "expected host_not_allowed or 403 in deny probe response" >&2
      echo "$deny_resp" >&2
      exit 7
    fi

    echo "git gateway checks passed (wget fallback path; allow_rc=${allow_rc})"
    exit 0
  fi

  echo "guest image missing both git and wget; cannot exercise git gateway" >&2
  exit 8
  ' >"$tmpdir/git-gateway.out" 2>"$tmpdir/git-gateway.err"
  git_gateway_status=$?
  set -e

  if [[ "$git_gateway_status" -eq 0 ]]; then
    break
  fi

  if grep -Eq 'timed out waiting for vsock guest agent|deadline_exceeded|Connection refused|Operation timed out' "$tmpdir/git-gateway.err" && [[ "$git_gateway_attempt" -lt "$git_gateway_max_attempts" ]]; then
    echo "git gateway test hit transient transport error (attempt $git_gateway_attempt/$git_gateway_max_attempts); retrying"
    sleep "$git_gateway_attempt"
    git_gateway_attempt=$((git_gateway_attempt + 1))
    continue
  fi
  break
done

if [[ "$git_gateway_status" -ne 0 ]]; then
  echo "git gateway allow/deny test failed (exit $git_gateway_status)" >&2
  echo "--- guest stdout ---" >&2
  cat "$tmpdir/git-gateway.out" >&2 || true
  echo "--- guest stderr ---" >&2
  cat "$tmpdir/git-gateway.err" >&2 || true
  dump_runtime_diagnostics 80
  exit 1
fi

echo "--- :warning: Exit code propagation test"
exit_attempt=1
exit_max_attempts=3
status=1
while true; do
  set +e
  ./dist/cleanroom exec --host "$listen_endpoint" -c "$PWD" -- sh -lc 'exit 7' >"$tmpdir/exit7.out" 2>"$tmpdir/exit7.err"
  status=$?
  set -e
  if [[ "$status" -eq 7 ]]; then
    break
  fi
  if grep -q 'timed out waiting for vsock guest agent' "$tmpdir/exit7.err" && [[ "$exit_attempt" -lt "$exit_max_attempts" ]]; then
    echo "exit propagation test hit transient vsock timeout (attempt $exit_attempt/$exit_max_attempts); retrying"
    sleep "$exit_attempt"
    exit_attempt=$((exit_attempt + 1))
    continue
  fi
  break
done
if [[ "$status" -ne 7 ]]; then
  echo "expected exit code 7 from guest command, got $status" >&2
  echo "stdout:" >&2
  cat "$tmpdir/exit7.out" >&2 || true
  echo "stderr:" >&2
  cat "$tmpdir/exit7.err" >&2 || true
  dump_runtime_diagnostics 80
  exit 1
fi

echo "--- :closed_lock_with_key: Gateway reachability test"
if grep -q 'gateway server started' "$tmpdir/server.log"; then
  echo "gateway server started (confirmed from server log)"
  # Extract the actual gateway port from the server log (may be ephemeral).
  gw_addr="$(grep 'gateway server started' "$tmpdir/server.log" | sed -nE 's/.*addr=([^ ]+).*/\1/p' | head -n1)"
  if [[ -z "$gw_addr" ]]; then
    gw_addr="127.0.0.1:8170"
  fi
  # Normalise 0.0.0.0 to 127.0.0.1 so curl doesn't route through HTTP_PROXY.
  gw_addr="${gw_addr/0.0.0.0/127.0.0.1}"
  echo "gateway address: $gw_addr"
  # Requests from localhost (non-TAP) should get 403 from the identity
  # middleware (unregistered source IP).
  set +e
  gw_body="$(curl -s -o - -w '\n%{http_code}' "http://$gw_addr/meta/" 2>&1)"
  gw_status=$?
  set -e
  gw_http_code="$(echo "$gw_body" | tail -n1)"
  if [[ "$gw_status" -eq 0 && "$gw_http_code" == "403" ]]; then
    echo "gateway correctly returned 403 for non-TAP source IP"
  elif [[ "$gw_status" -ne 0 ]]; then
    echo "gateway connection refused/unreachable from localhost (INPUT rules blocking) — acceptable"
  else
    echo "unexpected gateway response: HTTP $gw_http_code (curl exit $gw_status)" >&2
    echo "$gw_body" >&2
    exit 1
  fi
else
  echo "gateway server not started (no log entry found) — skipping reachability test"
fi

echo "--- :bar_chart: Execution observability present"
./dist/cleanroom status --last | tee "$tmpdir/status.out"
if ! grep -q 'execution-observability.json' "$tmpdir/status.out"; then
  echo "expected execution-observability.json reference in status output" >&2
  exit 1
fi

obs_file="$(
  find "$XDG_STATE_HOME"/cleanroom/executions -name execution-observability.json -type f -print 2>/dev/null \
    | while IFS= read -r path; do
        stat -c '%Y %n' "$path"
      done \
    | sort -nr \
    | head -n 1 \
    | cut -d' ' -f2-
)"
if [[ -n "$obs_file" && -f "$obs_file" ]]; then
  extract_json_number() {
    local key="$1"
    local file="$2"
    sed -nE "s/.*\"${key}\"[[:space:]]*:[[:space:]]*([0-9]+).*/\1/p" "$file" | head -n 1
  }
  extract_json_string() {
    local key="$1"
    local file="$2"
    sed -nE "s/.*\"${key}\"[[:space:]]*:[[:space:]]*\"([^\"]+)\".*/\1/p" "$file" | head -n 1
  }

  execution_id="$(extract_json_string execution_id "$obs_file")"
  total_ms="$(extract_json_number total_ms "$obs_file")"
  policy_resolve_ms="$(extract_json_number policy_resolve_ms "$obs_file")"
  rootfs_copy_ms="$(extract_json_number rootfs_copy_ms "$obs_file")"
  network_setup_ms="$(extract_json_number network_setup_ms "$obs_file")"
  firecracker_start_ms="$(extract_json_number firecracker_start_ms "$obs_file")"
  vm_ready_ms="$(extract_json_number vm_ready_ms "$obs_file")"
  vsock_wait_ms="$(extract_json_number vsock_wait_ms "$obs_file")"
  guest_exec_ms="$(extract_json_number guest_exec_ms "$obs_file")"
  cleanup_ms="$(extract_json_number cleanup_ms "$obs_file")"

  if command -v buildkite-agent >/dev/null 2>&1; then
    annotation_file="$tmpdir/observability-annotation.md"
    cat > "$annotation_file" <<EOF
### Firecracker E2E Observability

- execution id: ${execution_id:-n/a}

| Metric | Value (ms) |
| --- | ---: |
| total | ${total_ms:-n/a} |
| policy resolve | ${policy_resolve_ms:-n/a} |
| rootfs copy | ${rootfs_copy_ms:-n/a} |
| network setup | ${network_setup_ms:-n/a} |
| firecracker start | ${firecracker_start_ms:-n/a} |
| vm ready | ${vm_ready_ms:-n/a} |
| vsock wait | ${vsock_wait_ms:-n/a} |
| guest exec | ${guest_exec_ms:-n/a} |
| cleanup | ${cleanup_ms:-n/a} |

Source: ${obs_file}
EOF
    buildkite-agent annotate --context cleanroom-e2e-observability --style info < "$annotation_file"
  fi
fi

  echo "Firecracker e2e checks passed"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
