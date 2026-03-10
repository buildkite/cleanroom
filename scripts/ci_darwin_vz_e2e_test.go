package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestCiDarwinVZE2EBootstrapsLinuxGuestAgentBinary(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-darwin-vz-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-darwin-vz-e2e.sh: %v", err)
	}

	if !strings.Contains(string(content), "GOOS=linux GOARCH=\"$host_arch\" CGO_ENABLED=0 go build -trimpath -o \"dist/cleanroom-guest-agent-linux-$host_arch\" ./cmd/cleanroom-guest-agent") {
		t.Fatalf("expected ci-darwin-vz-e2e.sh to bootstrap cleanroom-guest-agent-linux-$host_arch")
	}
}
