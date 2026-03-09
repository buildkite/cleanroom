package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestCICleanroomScriptUsesNonInteractivePrivilegedFallbacks(t *testing.T) {
	t.Helper()

	scriptBytes, err := os.ReadFile("ci-cleanroom-e2e.sh")
	if err != nil {
		t.Fatalf("read script: %v", err)
	}

	script := string(scriptBytes)
	requiredSnippets := []string{
		"PRIVILEGED_HELPER_PATH=\"${CLEANROOM_PRIVILEGED_HELPER_PATH:-/usr/local/sbin/cleanroom-root-helper}\"",
		"sudo -n \"$@\"",
	}

	for _, snippet := range requiredSnippets {
		if !strings.Contains(script, snippet) {
			t.Fatalf("script is missing required privileged execution safeguard: %q", snippet)
		}
	}
}
