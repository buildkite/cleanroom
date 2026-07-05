#!/usr/bin/env bash
set -euo pipefail

CURRENT=$(svu current)
NEXT=$(svu next)

# While Cleanroom is pre-1.0, breaking product redesigns are represented as
# the next 0.x minor release rather than v1.0.0.
if [[ "$CURRENT" == v0.* && "$NEXT" == v1.* ]]; then
  NEXT=$(svu minor)
fi

if [ "$NEXT" = "$CURRENT" ]; then
  echo "No version bump detected (current: $CURRENT). Use conventional commits (feat:, fix:, etc.)."
  exit 1
fi
echo "Releasing $CURRENT -> $NEXT"
git tag "$NEXT"
git push origin "$NEXT"
echo "Tagged and pushed $NEXT — Buildkite release pipeline will publish the release."
