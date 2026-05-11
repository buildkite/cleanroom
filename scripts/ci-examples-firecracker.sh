#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

ROOT_HELPER_REQUIRED_CAPABILITIES=(
  firecracker-network
  firecracker-trusted-dns
)

run_privileged() {
  if [[ -z "${PRIVILEGED_HELPER_PATH:-}" ]]; then
    echo "CLEANROOM_PRIVILEGED_HELPER_PATH is required for Firecracker example CI" >&2
    return 1
  fi
  sudo -n "$PRIVILEGED_HELPER_PATH" "$@"
}

verify_helper_capabilities() {
  if [[ -z "${PRIVILEGED_HELPER_PATH:-}" ]]; then
    echo "CLEANROOM_PRIVILEGED_HELPER_PATH is required for Firecracker example CI" >&2
    return 1
  fi

  local capabilities
  if ! capabilities="$(sudo -n "$PRIVILEGED_HELPER_PATH" capabilities 2>&1)"; then
    echo "root helper capability probe failed via $PRIVILEGED_HELPER_PATH" >&2
    echo "$capabilities" >&2
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
    echo "root helper on host ($PRIVILEGED_HELPER_PATH) is missing required capabilities: ${missing[*]}" >&2
    return 1
  fi
}

purge_stale_cleanroom_resources() {
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

  local taps
  taps="$(run_privileged ip -o link show 2>/dev/null | grep -oP 'cr[a-z0-9]{1,13}(?=:)' || true)"
  for tap in $taps; do
    echo "removing stale tap device and iptables rules: $tap"
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

  local nat_rules
  nat_rules="$(run_privileged iptables -t nat -S POSTROUTING 2>/dev/null | grep 'MASQUERADE' | grep -E '10\.[0-9]+\.[0-9]+\.' || true)"
  while IFS= read -r rule; do
    [[ -n "$rule" ]] || continue
    # shellcheck disable=SC2086
    run_privileged iptables -t nat ${rule/-A/-D} 2>/dev/null || true
  done <<< "$nat_rules"
}

cleanup() {
  local status="$1"
  if [[ -n "${srv_pid:-}" ]]; then
    kill "$srv_pid" >/dev/null 2>&1 || true
    wait "$srv_pid" >/dev/null 2>&1 || true
  fi
  upload_firecracker_artifacts "$status" || true
  sleep 1
  purge_stale_cleanroom_resources 2>/dev/null || true
  rm -rf "$tmpdir"
}

upload_firecracker_artifacts() {
  local status="$1"
  if [[ -z "${BUILDKITE:-}" ]]; then
    return 0
  fi
  if ! command -v buildkite-agent >/dev/null 2>&1; then
    return 0
  fi
  if [[ -z "${tmpdir:-}" || ! -d "$tmpdir" ]]; then
    return 0
  fi

  local artifact_dir artifact_path
  artifact_dir="$(mktemp -d)"
  artifact_path="$(mktemp "/tmp/cleanroom-ci-examples-firecracker.XXXXXX.tgz")"

  cp "$tmpdir/server.log" "$artifact_dir/server.log" 2>/dev/null || true
  cp "$tmpdir/doctor.json" "$artifact_dir/doctor.json" 2>/dev/null || true
  if [[ -f "$tmpdir/config/cleanroom/config.yaml" ]]; then
    mkdir -p "$artifact_dir/config/cleanroom"
    cp "$tmpdir/config/cleanroom/config.yaml" "$artifact_dir/config/cleanroom/config.yaml" || true
  fi

  if tar -czf "$artifact_path" -C "$artifact_dir" .; then
    echo "--- :package: Upload firecracker example artifacts (status=$status)"
    buildkite-agent artifact upload "$artifact_path" || true
  fi

  rm -rf "$artifact_dir"
  rm -f "$artifact_path"
}

main() {
  KERNEL_IMAGE="${CLEANROOM_KERNEL_IMAGE:-}"
  FIRECRACKER_BINARY="${CLEANROOM_FIRECRACKER_BINARY:-firecracker}"
  PRIVILEGED_HELPER_PATH="${CLEANROOM_PRIVILEGED_HELPER_PATH:-/usr/local/sbin/cleanroom-root-helper}"

  verify_helper_capabilities

  echo "--- :broom: Pre-build cleanup"
  purge_stale_cleanroom_resources

  echo "--- :hammer: Building binaries"
  scripts/build-go.sh

  tmpdir="$(mktemp -d)"
  trap 'status=$?; cleanup "$status"; exit "$status"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

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
    vcpus: 2
    memory_mib: 1024
    launch_seconds: 90
EOF
  if [[ -n "$KERNEL_IMAGE" ]]; then
    echo "    kernel_image: $KERNEL_IMAGE" >> "$XDG_CONFIG_HOME/cleanroom/config.yaml"
  fi
  echo "    privileged_helper_path: $PRIVILEGED_HELPER_PATH" >> "$XDG_CONFIG_HOME/cleanroom/config.yaml"

  echo "--- :stethoscope: Doctor"
  ./dist/cleanroom doctor --json | tee "$tmpdir/doctor.json"
  if grep -q '"status": "fail"' "$tmpdir/doctor.json"; then
    echo "doctor checks reported failures" >&2
    exit 1
  fi

  socket_path="$tmpdir/cleanroom.sock"
  listen_endpoint="unix://$socket_path"

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
    cat "$tmpdir/server.log" >&2 || true
    exit 1
  fi

  "$SCRIPT_DIR/ci-example-smoke.sh" firecracker "$listen_endpoint" "$PWD"

  echo "firecracker example checks passed"
}

main "$@"
