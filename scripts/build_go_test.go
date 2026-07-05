package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestBuildGoBuildsCleanroomCLI(t *testing.T) {
	content, err := os.ReadFile("build-go.sh")
	if err != nil {
		t.Fatalf("read build-go.sh: %v", err)
	}

	script := string(content)
	if !strings.Contains(script, "go build -ldflags \"-X main.version=$VERSION\" -o dist/cleanroom ./cmd/cleanroom") {
		t.Fatalf("expected build-go.sh to build the cleanroom CLI")
	}
	if strings.Contains(script, "cleanroom-guest-agent") {
		t.Fatalf("build-go.sh still references the removed guest-agent binary")
	}
}
