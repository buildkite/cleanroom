#!/usr/bin/env bash
set -euo pipefail

VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
HOST_OS=$(go env GOOS)
HOST_ARCH=$(go env GOARCH)
mkdir -p dist
go build -ldflags "-X main.version=$VERSION" -o dist/cleanroom ./cmd/cleanroom
go build -o dist/download-sandbox-file ./scripts/download_sandbox_file
GOOS=linux GOARCH="$HOST_ARCH" CGO_ENABLED=0 go build -trimpath -o "dist/cleanroom-guest-agent-linux-$HOST_ARCH" ./cmd/cleanroom-guest-agent
if [[ "$HOST_OS" == "linux" ]]; then
  cp "dist/cleanroom-guest-agent-linux-$HOST_ARCH" dist/cleanroom-guest-agent
fi
