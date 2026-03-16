package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestCiCleanroomE2EUsesNonInteractiveSudo(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-cleanroom-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-cleanroom-e2e.sh: %v", err)
	}

	if !strings.Contains(string(content), "sudo -n \"$@\"") {
		t.Fatalf("expected ci-cleanroom-e2e.sh to use non-interactive sudo (-n)")
	}
	if !strings.Contains(string(content), "sudo -n \"$PRIVILEGED_HELPER_PATH\" \"$@\"") {
		t.Fatalf("expected ci-cleanroom-e2e.sh to invoke the privileged helper via non-interactive sudo (-n)")
	}
}

func TestCiCleanroomE2EDownloadSandboxFileProbeUsesTimeout(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-cleanroom-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-cleanroom-e2e.sh: %v", err)
	}

	if !strings.Contains(string(content), "--timeout 45s") {
		t.Fatalf("expected ci-cleanroom-e2e.sh to bound download_sandbox_file calls with --timeout")
	}
}

func TestCiCleanroomE2EDocumentsTrustedHelperRollout(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-cleanroom-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-cleanroom-e2e.sh: %v", err)
	}

	if !strings.Contains(string(content), "scripts/update-cleanroom-root-helper.sh") {
		t.Fatalf("expected ci-cleanroom-e2e.sh to direct helper updates through scripts/update-cleanroom-root-helper.sh")
	}
}
