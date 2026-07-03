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
	if !strings.Contains(script, "go build -tags libspore -ldflags \"-X main.version=$VERSION\" -o dist/cleanroom ./cmd/cleanroom") {
		t.Fatalf("expected build-go.sh to build the cleanroom CLI with libspore support")
	}
	if !strings.Contains(script, "HOST_ARCH=$(go env GOARCH)") {
		t.Fatalf("expected build-go.sh to resolve the host architecture for linux guest-agent cross-compiles")
	}
	if !strings.Contains(script, "GOOS=linux GOARCH=\"$HOST_ARCH\" CGO_ENABLED=0 go build -trimpath -o \"dist/cleanroom-guest-agent-linux-$HOST_ARCH\" ./cmd/cleanroom-guest-agent") {
		t.Fatalf("expected build-go.sh to bootstrap cleanroom-guest-agent-linux-$HOST_ARCH")
	}
}
