#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=scripts/dist-layout.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/dist-layout.sh"

VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
HOST_OS=$(go env GOOS)
HOST_ARCH=$(go env GOARCH)
BIN_DIR="$(cleanroom_stage_bin_dir "$HOST_OS" "$HOST_ARCH")"
LIBEXEC_DIR="$(cleanroom_stage_libexec_dir "$HOST_OS" "$HOST_ARCH")"
mkdir -p "$BIN_DIR" "$LIBEXEC_DIR"
go build -ldflags "-X main.version=$VERSION" -o "$BIN_DIR/cleanroom" ./cmd/cleanroom
go build -o "$BIN_DIR/download-sandbox-file" ./scripts/download_sandbox_file
GOOS=linux GOARCH="$HOST_ARCH" CGO_ENABLED=0 go build -trimpath -o "$LIBEXEC_DIR/cleanroom-guest-agent-linux-$HOST_ARCH" ./cmd/cleanroom-guest-agent
