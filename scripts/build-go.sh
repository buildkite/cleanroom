#!/usr/bin/env bash
set -euo pipefail

VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
mkdir -p dist
go build -ldflags "-X main.version=$VERSION" -o dist/cleanroom ./cmd/cleanroom
go build -o dist/cleanroom-guest-agent ./cmd/cleanroom-guest-agent
