package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestBuildGoBootstrapsLinuxGuestAgentBinary(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("build-go.sh")
	if err != nil {
		t.Fatalf("read build-go.sh: %v", err)
	}

	script := string(content)
	if !strings.Contains(script, `source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/dist-layout.sh"`) {
		t.Fatalf("expected build-go.sh to source the staged dist layout helpers")
	}
	if !strings.Contains(script, `STAGE_DIR="$(cleanroom_stage_dir "$HOST_OS" "$HOST_ARCH")"`) {
		t.Fatalf("expected build-go.sh to stage outputs under dist/<os>-<arch>/")
	}
	if !strings.Contains(script, "HOST_ARCH=$(go env GOARCH)") {
		t.Fatalf("expected build-go.sh to resolve the host architecture for linux guest-agent cross-compiles")
	}
	if !strings.Contains(script, `go build -o "$BIN_DIR/download-sandbox-file" ./scripts/download_sandbox_file`) {
		t.Fatalf("expected build-go.sh to stage download-sandbox-file under dist/<os>-<arch>/bin")
	}
	if !strings.Contains(script, `GOOS=linux GOARCH="$HOST_ARCH" CGO_ENABLED=0 go build -trimpath -o "$LIBEXEC_DIR/cleanroom-guest-agent-linux-$HOST_ARCH" ./cmd/cleanroom-guest-agent`) {
		t.Fatalf("expected build-go.sh to stage cleanroom-guest-agent-linux-$HOST_ARCH under dist/<os>-<arch>/libexec/cleanroom")
	}
}
