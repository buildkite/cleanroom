package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestE2EObservabilityHelperPublishesAnnotationsAndArtifacts(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("e2e-observability.sh")
	if err != nil {
		t.Fatalf("read e2e-observability.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		`capture_latest_execution_observability()`,
		`require_launch_observability()`,
		`write_observability_annotation()`,
		`copy_execution_observability_payloads()`,
		`publish_buildkite_observability()`,
		`execution inspect --host "$listen_endpoint" --json`,
		`buildkite-agent annotate --context "$annotation_context" --style info <"$annotation_path"`,
		`buildkite-agent artifact upload "$archive_name"`,
		`launch-execution-observability.json`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected e2e-observability.sh to contain %q", needle)
		}
	}
}
