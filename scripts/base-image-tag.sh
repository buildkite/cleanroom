#!/usr/bin/env bash
set -euo pipefail

# Deterministic tag derived from image definition inputs.
hash="$(sha256sum Dockerfile.base-image | awk '{print $1}')"
printf 'alpine-%s\n' "${hash:0:12}"
