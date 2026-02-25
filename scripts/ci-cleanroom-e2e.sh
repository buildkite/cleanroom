#!/usr/bin/env bash
set -euo pipefail

KERNEL_IMAGE="${CLEANROOM_KERNEL_IMAGE:-}"
ROOTFS_IMAGE="${CLEANROOM_ROOTFS:-}"
FIRECRACKER_BINARY="${CLEANROOM_FIRECRACKER_BINARY:-firecracker}"
PRIVILEGED_MODE="${CLEANROOM_PRIVILEGED_MODE:-}"
PRIVILEGED_HELPER_PATH="${CLEANROOM_PRIVILEGED_HELPER_PATH:-}"

if [[ -z "$KERNEL_IMAGE" ]]; then
  echo "CLEANROOM_KERNEL_IMAGE is required for Firecracker e2e CI" >&2
  exit 1
fi
if [[ -z "$ROOTFS_IMAGE" ]]; then
  echo "CLEANROOM_ROOTFS is required for Firecracker e2e CI" >&2
  exit 1
fi

# run_privileged executes a privileged command via the root helper,
# falling back to sudo, then direct execution.
run_privileged() {
  if [[ -n "${PRIVILEGED_HELPER_PATH:-}" ]]; then
    "$PRIVILEGED_HELPER_PATH" "$@"
  elif command -v sudo >/dev/null 2>&1; then
    sudo "$@"
  else
    "$@"
  fi
}

# purge_stale_cleanroom_resources removes TAP devices, iptables rules,
# firecracker processes, and temp mount dirs left over from a previous
# run that crashed before cleanup.
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

  # Unmount and remove stale rootfs temp dirs.
  for mnt in /tmp/cleanroom-firecracker-rootfs-*; do
    [[ -d "$mnt" ]] || continue
    echo "cleaning stale mount: $mnt"
    run_privileged umount "$mnt" 2>/dev/null || true
    rm -rf "$mnt" 2>/dev/null || true
  done
}

echo "--- :broom: Pre-build cleanup"
purge_stale_cleanroom_resources

echo "--- :hammer: Building binaries"
mise run build

tmpdir="$(mktemp -d)"
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
trap cleanup EXIT

export XDG_CONFIG_HOME="$tmpdir/config"
export XDG_STATE_HOME="$tmpdir/state"
export XDG_RUNTIME_DIR="$tmpdir/runtime"
export XDG_DATA_HOME="$tmpdir/data"

mkdir -p "$XDG_CONFIG_HOME" "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR" "$XDG_DATA_HOME"
mkdir -p "$XDG_CONFIG_HOME/cleanroom"
cat > "$XDG_CONFIG_HOME/cleanroom/config.yaml" <<EOF
default_backend: firecracker
backends:
  firecracker:
    binary_path: $FIRECRACKER_BINARY
    kernel_image: $KERNEL_IMAGE
    rootfs: $ROOTFS_IMAGE
    vcpus: 2
    memory_mib: 1024
    launch_seconds: 45
EOF

if [[ -n "$PRIVILEGED_MODE" ]]; then
  echo "    privileged_mode: $PRIVILEGED_MODE" >> "$XDG_CONFIG_HOME/cleanroom/config.yaml"
fi
if [[ -n "$PRIVILEGED_HELPER_PATH" ]]; then
  echo "    privileged_helper_path: $PRIVILEGED_HELPER_PATH" >> "$XDG_CONFIG_HOME/cleanroom/config.yaml"
fi

if [[ -n "$PRIVILEGED_HELPER_PATH" && -f "scripts/cleanroom-root-helper.sh" ]]; then
  repo_sha="$(sha256sum scripts/cleanroom-root-helper.sh | awk '{print $1}')"
  host_sha="$(sha256sum "$PRIVILEGED_HELPER_PATH" 2>/dev/null | awk '{print $1}')" || true
  if [[ -n "$host_sha" && "$repo_sha" != "$host_sha" ]]; then
    echo "⚠️  Root helper on host ($PRIVILEGED_HELPER_PATH) does not match repo (scripts/cleanroom-root-helper.sh)"
    echo "   host: $host_sha"
    echo "   repo: $repo_sha"
    echo "   Update with: sudo install -o root -g root -m 0755 scripts/cleanroom-root-helper.sh $PRIVILEGED_HELPER_PATH"
    if command -v buildkite-agent >/dev/null 2>&1; then
      buildkite-agent annotate --context root-helper-drift --style error <<EOF
### ❌ Root helper out of date

The installed root helper (\`$PRIVILEGED_HELPER_PATH\`) does not match \`scripts/cleanroom-root-helper.sh\` from this commit.

| | SHA-256 |
|---|---|
| Host | \`$host_sha\` |
| Repo | \`$repo_sha\` |

**Update on the CI host:**
\`\`\`
sudo install -o root -g root -m 0755 scripts/cleanroom-root-helper.sh $PRIVILEGED_HELPER_PATH
\`\`\`
EOF
    fi
    exit 1
  fi
fi

echo "--- :stethoscope: Doctor"
./dist/cleanroom doctor --json | tee "$tmpdir/doctor.json"
if grep -q '"status": "fail"' "$tmpdir/doctor.json"; then
  echo "doctor checks reported failures" >&2
  exit 1
fi

socket_path="$tmpdir/cleanroom.sock"
listen_endpoint="unix://$socket_path"

echo "--- :rocket: Start cleanroom control-plane"
./dist/cleanroom serve --listen "$listen_endpoint" --gateway-listen "127.0.0.1:0" >"$tmpdir/server.log" 2>&1 &
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
./dist/cleanroom exec --host "$listen_endpoint" -c "$PWD" -- sh -lc 'echo cleanroom-e2e' | tee "$tmpdir/exec.out"
if ! grep -q '^cleanroom-e2e$' "$tmpdir/exec.out"; then
  echo "expected smoke-test output missing" >&2
  exit 1
fi

echo "--- :warning: Exit code propagation test"
set +e
./dist/cleanroom exec --host "$listen_endpoint" -c "$PWD" -- sh -lc 'exit 7' >"$tmpdir/exit7.out" 2>"$tmpdir/exit7.err"
status=$?
set -e
if [[ "$status" -ne 7 ]]; then
  echo "expected exit code 7 from guest command, got $status" >&2
  echo "stdout:" >&2
  cat "$tmpdir/exit7.out" >&2 || true
  echo "stderr:" >&2
  cat "$tmpdir/exit7.err" >&2 || true
  echo "server log (last 30 lines):" >&2
  tail -n 30 "$tmpdir/server.log" >&2 || true
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

echo "--- :bar_chart: Run observability present"
./dist/cleanroom status --last-run | tee "$tmpdir/status.out"
if ! grep -q 'run-observability.json' "$tmpdir/status.out"; then
  echo "expected run-observability.json reference in status output" >&2
  exit 1
fi

obs_file="$(ls -1t "$XDG_STATE_HOME"/cleanroom/runs/*/run-observability.json 2>/dev/null | head -n 1 || true)"
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

  run_id="$(extract_json_string run_id "$obs_file")"
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

- run id: ${run_id:-n/a}

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
