#!/usr/bin/env bash
set -euo pipefail

image_name="${1:-alpine}"
dockerfile="${2:-Dockerfile.base-image}"

if [[ ! -f "$dockerfile" ]]; then
  echo "dockerfile not found: $dockerfile" >&2
  exit 1
fi

# Deterministic tag derived from image definition inputs.
hash="$(sha256sum "$dockerfile" | awk '{print $1}')"
printf '%s-%s\n' "$image_name" "${hash:0:12}"
