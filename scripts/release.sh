#!/usr/bin/env bash
set -euo pipefail

NEXT=$(svu next)
CURRENT=$(svu current)
if [ "$NEXT" = "$CURRENT" ]; then
  echo "No version bump detected (current: $CURRENT). Use conventional commits (feat:, fix:, etc.)."
  exit 1
fi
echo "Releasing $CURRENT -> $NEXT"
git tag "$NEXT"
git push origin "$NEXT"
echo "Tagged and pushed $NEXT — release workflow will run on GitHub."
