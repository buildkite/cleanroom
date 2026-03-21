#!/usr/bin/env bash

cleanroom_dist_root() {
  printf '%s\n' "${CLEANROOM_DIST_DIR:-dist}"
}

cleanroom_host_goos() {
  go env GOOS
}

cleanroom_host_goarch() {
  go env GOARCH
}

cleanroom_stage_dir() {
  local goos="${1:-$(cleanroom_host_goos)}"
  local goarch="${2:-$(cleanroom_host_goarch)}"
  printf '%s/%s-%s\n' "$(cleanroom_dist_root)" "$goos" "$goarch"
}

cleanroom_stage_bin_dir() {
  printf '%s/bin\n' "$(cleanroom_stage_dir "$@")"
}

cleanroom_stage_libexec_dir() {
  printf '%s/libexec/cleanroom\n' "$(cleanroom_stage_dir "$@")"
}

cleanroom_stage_bin_path() {
  local name="$1"
  shift || true
  printf '%s/%s\n' "$(cleanroom_stage_bin_dir "$@")" "$name"
}

cleanroom_stage_libexec_path() {
  local name="$1"
  shift || true
  printf '%s/%s\n' "$(cleanroom_stage_libexec_dir "$@")" "$name"
}

cleanroom_prefix_bin_dir() {
  local prefix="$1"
  printf '%s/bin\n' "$prefix"
}

cleanroom_prefix_libexec_dir() {
  local prefix="$1"
  printf '%s/libexec/cleanroom\n' "$prefix"
}
