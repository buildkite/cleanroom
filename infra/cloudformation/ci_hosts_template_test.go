package cloudformation_test

import (
	"os"
	"strings"
	"testing"
)

func TestLinuxBootstrapEnsuresAgentOwnsLocalTree(t *testing.T) {
	t.Helper()

	templateBytes, err := os.ReadFile("ci-hosts.yaml")
	if err != nil {
		t.Fatalf("read ci-hosts template: %v", err)
	}

	template := string(templateBytes)
	requiredSnippets := []string{
		"install -d -o buildkite-agent -g buildkite-agent -m 0755 /var/lib/buildkite-agent/.local",
		"install -d -o buildkite-agent -g buildkite-agent -m 0755 /var/lib/buildkite-agent/.local/share",
		"install -d -o buildkite-agent -g buildkite-agent -m 0755 /var/lib/buildkite-agent/.local/share/cleanroom",
	}

	for _, snippet := range requiredSnippets {
		if !strings.Contains(template, snippet) {
			t.Fatalf("linux bootstrap is missing required path ownership step: %q", snippet)
		}
	}
}
