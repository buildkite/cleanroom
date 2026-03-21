#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=scripts/dist-layout.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/dist-layout.sh"

VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
HOST_OS=$(go env GOOS)
HOST_ARCH=$(go env GOARCH)
DIST_DIR="$(cleanroom_dist_root)"
BIN_DIR="$(cleanroom_stage_bin_dir "$HOST_OS" "$HOST_ARCH")"
LIBEXEC_DIR="$(cleanroom_stage_libexec_dir "$HOST_OS" "$HOST_ARCH")"
GUEST_AGENT_NAME="cleanroom-guest-agent-linux-$HOST_ARCH"
LEGACY_GUEST_AGENT_PATH="$DIST_DIR/$GUEST_AGENT_NAME"
LEGACY_GENERIC_GUEST_AGENT_PATH="$DIST_DIR/cleanroom-guest-agent"
mkdir -p "$DIST_DIR" "$BIN_DIR" "$LIBEXEC_DIR"
go build -ldflags "-X main.version=$VERSION" -o "$BIN_DIR/cleanroom" ./cmd/cleanroom
go build -o "$BIN_DIR/download-sandbox-file" ./scripts/download_sandbox_file
GOOS=linux GOARCH="$HOST_ARCH" CGO_ENABLED=0 go build -trimpath -o "$LIBEXEC_DIR/$GUEST_AGENT_NAME" ./cmd/cleanroom-guest-agent
install -m 0755 "$LIBEXEC_DIR/$GUEST_AGENT_NAME" "$LEGACY_GUEST_AGENT_PATH"
if [[ "$HOST_OS" == "linux" ]]; then
  install -m 0755 "$LIBEXEC_DIR/$GUEST_AGENT_NAME" "$LEGACY_GENERIC_GUEST_AGENT_PATH"
fi
