#!/usr/bin/env bash
set -euo pipefail

log() {
  printf '[update-cleanroom-root-helper] %s\n' "$*"
}

die() {
  printf '[update-cleanroom-root-helper] error: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: scripts/update-cleanroom-root-helper.sh [trusted-ref]

Install scripts/cleanroom-root-helper.sh from a ref that is reachable from
origin/main. The default trusted ref is origin/main.
EOF
}

require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || die "required command not found: ${cmd}"
}

require_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    die "must run as root"
  fi
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

if [[ "$#" -gt 1 ]]; then
  usage >&2
  exit 1
fi

require_root
require_cmd git
require_cmd install

helper_install_path="${CLEANROOM_HELPER_INSTALL_PATH:-/usr/local/sbin/cleanroom-root-helper}"
trusted_ref="${1:-${CLEANROOM_ROOT_HELPER_REF:-origin/main}}"

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/.." && pwd)"
tmp_helper="$(mktemp)"
trap 'rm -f "$tmp_helper"' EXIT

cd "$repo_root"
git fetch --quiet origin main

trusted_commit="$(git rev-parse --verify "${trusted_ref}^{commit}")" || die "unable to resolve trusted ref ${trusted_ref}"
origin_main_commit="$(git rev-parse --verify "origin/main^{commit}")" || die "unable to resolve origin/main"
git merge-base --is-ancestor "$trusted_commit" "$origin_main_commit" || die "trusted ref ${trusted_ref} is not reachable from origin/main"

git show "$trusted_commit:scripts/cleanroom-root-helper.sh" >"$tmp_helper" || die "unable to read scripts/cleanroom-root-helper.sh from ${trusted_ref}"
install -o root -g root -m 0755 "$tmp_helper" "$helper_install_path"

log "installed cleanroom root helper from ${trusted_commit} to ${helper_install_path}"
