#!/usr/bin/env bash
# End-to-end bake smoke: exercises the plan's Slice 3-5 runtime path against a
# real spore CLI. Requires spore on PATH and a working hypervisor (hvf on
# macOS, kvm on Linux). Builds the cleanroom CLI, bakes a warm spore from a
# fixture repo, restores it with spore run --from, forks it, and verifies
# provenance.
set -euo pipefail

SPORE_BIN="${SPORE_BIN:-spore}"
BASE_IMAGE="${CLEANROOM_SMOKE_IMAGE:-ghcr.io/buildkite/cleanroom-base/alpine@sha256:91a63856cdf97b2e5659660b41d1a131d3b57bfa4cad254018e391ffef6fa4b9}"

command -v "$SPORE_BIN" >/dev/null 2>&1 || {
  echo "ci-bake-smoke: spore not found on PATH (set SPORE_BIN)" >&2
  exit 1
}

echo "--- Build cleanroom CLI"
mkdir -p dist
go build -o dist/cleanroom ./cmd/cleanroom

cleanroom() { ./dist/cleanroom "$@"; }

workdir="$(mktemp -d "${TMPDIR:-/tmp}/cleanroom-bake-smoke.XXXXXX")"
repo="${workdir}/repo"
# The artifact lives inside the repo, matching the documented
# `cleanroom bake . --out repo.spore` flow: rebake idempotency and
# `verify --dir` must not mistake the artifact for uncommitted source.
spore_out="${repo}/repo.spore"
forks="${workdir}/forks"
mkdir -p "$repo"

cleanup() {
  "$SPORE_BIN" ls 2>/dev/null | awk 'NR>1 && $1 ~ /^cr-bake-/ {print $1}' | while read -r vm; do
    "$SPORE_BIN" rm "$vm" >/dev/null 2>&1 || true
  done
  rm -rf "$workdir"
}
trap cleanup EXIT

echo "--- Write fixture repo"
cat > "${repo}/cleanroom.yaml" <<EOF
version: 1
sandbox:
  image:
    ref: ${BASE_IMAGE}
  resources:
    memory: 1gb
  warmup:
    - "install -m 0755 vendor/run-tests /usr/local/bin/run-tests"
EOF
mkdir -p "${repo}/vendor"
# shellcheck disable=SC2016  # $(hostname) is meant to run in the guest, not here
printf '#!/bin/sh\necho tests-passed on $(hostname)\n' > "${repo}/vendor/run-tests"
git -C "$repo" init -q
git -C "$repo" add -A
git -C "$repo" -c user.email=ci@example.com -c user.name="CI Example" -c commit.gpgsign=false commit -qm init

echo "--- Bake"
cleanroom bake "$repo" --out "$spore_out" --spore "$SPORE_BIN"

echo "--- Restore with spore run --from"
out="$("$SPORE_BIN" run --from "$spore_out" 'run-tests')"
echo "$out"
case "$out" in
  *tests-passed*) ;;
  *) echo "ci-bake-smoke: run --from did not use the baked dependency" >&2; exit 1 ;;
esac

echo "--- Rebake must no-op"
cleanroom bake "$repo" --out "$spore_out" --spore "$SPORE_BIN" 2>&1 | tee "${workdir}/rebake.log"
grep -q "up to date" "${workdir}/rebake.log" || {
  echo "ci-bake-smoke: clean rebake was not idempotent" >&2
  exit 1
}

echo "--- Fork the artifact"
"$SPORE_BIN" fork "$spore_out" --count 2 --out "$forks"
# shellcheck disable=SC2012  # fork children are numeric dirs (000000, ...)
child="$(ls "$forks" | head -1)"
"$SPORE_BIN" run --from "${forks}/${child}" 'run-tests' | grep -q tests-passed || {
  echo "ci-bake-smoke: forked child could not run the baked dependency" >&2
  exit 1
}

echo "--- Verify provenance"
cleanroom verify "$spore_out" --dir "$repo" --spore "$SPORE_BIN"

echo "ci-bake-smoke ok"
