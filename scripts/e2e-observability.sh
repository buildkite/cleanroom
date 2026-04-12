#!/usr/bin/env bash

# Capture the most recent retained execution immediately after a launched run so
# later sandbox reuse checks do not overwrite the interesting observability.
capture_latest_execution_observability() {
  local cleanroom_bin="${1:-./dist/cleanroom}"
  local status_output=""

  status_output="$("$cleanroom_bin" status --last 2>/dev/null || true)"
  CLEANROOM_E2E_LAUNCH_STATUS_OUTPUT="$status_output"
  CLEANROOM_E2E_LAUNCH_EXECUTION_ID="$(printf '%s\n' "$status_output" | sed -n 's#execution artifacts: .*/\([^/]*\)$#\1#p' | head -n 1)"
  CLEANROOM_E2E_LAUNCH_OBSERVABILITY_PATH="$(printf '%s\n' "$status_output" | sed -n 's#observability (\(.*execution-observability\.json\)):#\1#p' | head -n 1)"
}

require_launch_observability() {
  local suite_label="$1"

  if [[ -z "${CLEANROOM_E2E_LAUNCH_EXECUTION_ID:-}" ]]; then
    echo "${suite_label} did not capture a launch execution id" >&2
    return 1
  fi
  if [[ -z "${CLEANROOM_E2E_LAUNCH_OBSERVABILITY_PATH:-}" || ! -f "${CLEANROOM_E2E_LAUNCH_OBSERVABILITY_PATH}" ]]; then
    echo "${suite_label} did not retain launch execution observability" >&2
    return 1
  fi
}

json_string_field() {
  local key="$1"
  local file="$2"
  sed -nE "s/.*\"${key}\"[[:space:]]*:[[:space:]]*\"([^\"]+)\".*/\1/p" "$file" | head -n 1
}

json_number_field() {
  local key="$1"
  local file="$2"
  sed -nE "s/.*\"${key}\"[[:space:]]*:[[:space:]]*([0-9]+).*/\1/p" "$file" | head -n 1
}

json_bool_field() {
  local key="$1"
  local file="$2"
  sed -nE "s/.*\"${key}\"[[:space:]]*:[[:space:]]*(true|false).*/\1/p" "$file" | head -n 1
}

append_metric_row() {
  local rows_file="$1"
  local key="$2"
  local label="$3"
  local file="$4"
  local value
  value="$(json_number_field "$key" "$file")"
  if [[ -n "$value" ]]; then
    printf '| %s | %s |\n' "$label" "$value" >>"$rows_file"
  fi
}

append_detail_row() {
  local rows_file="$1"
  local label="$2"
  local value="$3"
  if [[ -n "$value" ]]; then
    printf "| %s | \`%s\` |\n" "$label" "$value" >>"$rows_file"
  fi
}

append_helper_timing_rows() {
  local rows_file="$1"
  local file="$2"

  awk '
    /"helper_timing_ms"[[:space:]]*:[[:space:]]*{/ {
      in_helper=1
      next
    }
    in_helper && /^[[:space:]]*}[,]?[[:space:]]*$/ {
      in_helper=0
      exit
    }
    in_helper {
      if (match($0, /"([^"]+)"[[:space:]]*:[[:space:]]*([0-9]+)/, m)) {
        printf("| helper %s | %s |\n", m[1], m[2])
      }
    }
  ' "$file" >>"$rows_file"
}

write_observability_annotation() {
  local suite_label="$1"
  local archive_name="$2"
  local obs_file="$3"
  local annotation_path="$4"

  local metric_rows
  local detail_rows
  metric_rows="$(mktemp)"
  detail_rows="$(mktemp)"

  append_metric_row "$metric_rows" "total_ms" "total" "$obs_file"
  append_metric_row "$metric_rows" "policy_resolve_ms" "policy resolve" "$obs_file"
  append_metric_row "$metric_rows" "rootfs_copy_ms" "rootfs copy" "$obs_file"
  append_metric_row "$metric_rows" "network_setup_ms" "network setup" "$obs_file"
  append_metric_row "$metric_rows" "firecracker_start_ms" "firecracker start" "$obs_file"
  append_metric_row "$metric_rows" "vm_ready_ms" "vm ready" "$obs_file"
  append_metric_row "$metric_rows" "vsock_wait_ms" "vsock wait" "$obs_file"
  append_metric_row "$metric_rows" "guest_exec_ms" "guest exec" "$obs_file"
  append_metric_row "$metric_rows" "cleanup_ms" "cleanup" "$obs_file"
  append_helper_timing_rows "$metric_rows" "$obs_file"

  append_detail_row "$detail_rows" "backend" "$(json_string_field backend "$obs_file")"
  append_detail_row "$detail_rows" "image ref" "$(json_string_field image_ref "$obs_file")"
  append_detail_row "$detail_rows" "image digest" "$(json_string_field image_digest "$obs_file")"
  append_detail_row "$detail_rows" "image cache hit" "$(json_bool_field image_cache_hit "$obs_file")"
  append_detail_row "$detail_rows" "network mode" "$(json_string_field network_mode "$obs_file")"
  append_detail_row "$detail_rows" "network tap" "$(json_string_field network_tap "$obs_file")"
  append_detail_row "$detail_rows" "guest ip" "$(json_string_field network_guest_ip "$obs_file")"
  append_detail_row "$detail_rows" "host ip" "$(json_string_field network_host_ip "$obs_file")"
  append_detail_row "$detail_rows" "gateway ip" "$(json_string_field network_gateway_ip "$obs_file")"
  append_detail_row "$detail_rows" "subnet" "$(json_string_field network_subnet_cidr "$obs_file")"
  append_detail_row "$detail_rows" "launch mode" "$(json_bool_field launched_vm "$obs_file")"
  append_detail_row "$detail_rows" "phase" "$(json_string_field phase "$obs_file")"
  append_detail_row "$detail_rows" "exit code" "$(json_number_field exit_code "$obs_file")"
  append_detail_row "$detail_rows" "error" "$(json_string_field error "$obs_file")"
  append_detail_row "$detail_rows" "guest error" "$(json_string_field guest_error "$obs_file")"

  {
    printf '### %s Observability\n\n' "$suite_label"
    printf -- "- execution id: \`%s\`\n" "$(json_string_field execution_id "$obs_file")"
    printf -- "- bundle: \`%s\` (doctor report, server log, execution payloads)\n" "$archive_name"
    printf -- "- source: \`%s\`\n" "$obs_file"

    if [[ -s "$metric_rows" ]]; then
      printf '\n| Metric | Value (ms) |\n'
      printf '| --- | ---: |\n'
      cat "$metric_rows"
    fi

    if [[ -s "$detail_rows" ]]; then
      printf '\n| Detail | Value |\n'
      printf '| --- | --- |\n'
      cat "$detail_rows"
    fi
  } >"$annotation_path"

  rm -f "$metric_rows" "$detail_rows"
}

write_missing_observability_annotation() {
  local suite_label="$1"
  local archive_name="$2"
  local annotation_path="$3"

  cat >"$annotation_path" <<EOF
### ${suite_label} Observability

- execution id: not captured
- bundle: \`${archive_name}\` (doctor report, server log, execution payloads)
- source: no retained launch execution observability file was found
EOF
}

copy_execution_observability_payloads() {
  local bundle_dir="$1"
  local source_dir="${XDG_STATE_HOME:-}/cleanroom/executions"
  local path=""
  local execution_id=""

  mkdir -p "$bundle_dir/executions"
  if [[ -d "$source_dir" ]]; then
    find "$source_dir" -name execution-observability.json -type f -print 2>/dev/null | while IFS= read -r path; do
      execution_id="$(basename "$(dirname "$path")")"
      mkdir -p "$bundle_dir/executions/$execution_id"
      cp "$path" "$bundle_dir/executions/$execution_id/execution-observability.json"
    done
  fi
}

publish_buildkite_observability() {
  local suite_label="$1"
  local annotation_context="$2"
  local archive_name="$3"
  local cleanroom_bin="${4:-./dist/cleanroom}"
  local listen_endpoint="${5:-}"
  local tmpdir="${6:-}"

  if [[ "${CLEANROOM_E2E_OBSERVABILITY_PUBLISHED:-0}" == "1" ]]; then
    return 0
  fi
  CLEANROOM_E2E_OBSERVABILITY_PUBLISHED=1

  if ! command -v buildkite-agent >/dev/null 2>&1; then
    return 0
  fi
  if [[ -z "$tmpdir" || ! -d "$tmpdir" ]]; then
    return 0
  fi
  if [[ -z "${CLEANROOM_E2E_LAUNCH_OBSERVABILITY_PATH:-}" ]]; then
    capture_latest_execution_observability "$cleanroom_bin"
  fi

  local bundle_dir="$tmpdir/buildkite-observability"
  local archive_path="$tmpdir/$archive_name"
  local annotation_path="$bundle_dir/annotation.md"
  mkdir -p "$bundle_dir"

  if [[ -f "$tmpdir/doctor.json" ]]; then
    cp "$tmpdir/doctor.json" "$bundle_dir/doctor.json"
  fi
  if [[ -f "$tmpdir/server.log" ]]; then
    cp "$tmpdir/server.log" "$bundle_dir/server.log"
  fi
  if [[ -n "${CLEANROOM_E2E_LAUNCH_STATUS_OUTPUT:-}" ]]; then
    printf '%s\n' "${CLEANROOM_E2E_LAUNCH_STATUS_OUTPUT}" >"$bundle_dir/status-last.txt"
  fi
  if [[ -n "${CLEANROOM_E2E_LAUNCH_EXECUTION_ID:-}" && -n "$listen_endpoint" ]]; then
    "$cleanroom_bin" execution inspect --host "$listen_endpoint" --json "${CLEANROOM_E2E_LAUNCH_EXECUTION_ID}" >"$bundle_dir/launch-execution-inspect.json" 2>"$bundle_dir/launch-execution-inspect.stderr" || true
  fi
  if [[ -n "${CLEANROOM_E2E_LAUNCH_OBSERVABILITY_PATH:-}" && -f "${CLEANROOM_E2E_LAUNCH_OBSERVABILITY_PATH}" ]]; then
    cp "${CLEANROOM_E2E_LAUNCH_OBSERVABILITY_PATH}" "$bundle_dir/launch-execution-observability.json"
    write_observability_annotation "$suite_label" "$archive_name" "${CLEANROOM_E2E_LAUNCH_OBSERVABILITY_PATH}" "$annotation_path"
  else
    write_missing_observability_annotation "$suite_label" "$archive_name" "$annotation_path"
  fi

  copy_execution_observability_payloads "$bundle_dir"

  tar -czf "$archive_path" -C "$bundle_dir" .
  buildkite-agent annotate --context "$annotation_context" --style info <"$annotation_path" || true
  (
    cd "$tmpdir"
    buildkite-agent artifact upload "$archive_name"
  ) || true
}
