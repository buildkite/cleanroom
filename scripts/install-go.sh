#!/usr/bin/env bash
set -euo pipefail

VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
BIN_DIR="$(go env GOBIN)"
if [ -z "$BIN_DIR" ]; then BIN_DIR="$(go env GOPATH)/bin"; fi
HOST_ARCH="$(go env GOARCH)"
mkdir -p "$BIN_DIR"
go install -ldflags "-X main.version=$VERSION" ./cmd/cleanroom ./cmd/cleanroom-guest-agent
GOOS=linux GOARCH="$HOST_ARCH" CGO_ENABLED=0 go build -trimpath -o "$BIN_DIR/cleanroom-guest-agent-linux-$HOST_ARCH" ./cmd/cleanroom-guest-agent
