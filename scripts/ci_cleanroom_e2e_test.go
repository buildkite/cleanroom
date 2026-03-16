package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestCiCleanroomE2EUsesHelperViaNonInteractiveSudo(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-cleanroom-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-cleanroom-e2e.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		"PRIVILEGED_HELPER_PATH=\"${CLEANROOM_PRIVILEGED_HELPER_PATH:-/usr/local/sbin/cleanroom-root-helper}\"",
		"sudo -n \"$PRIVILEGED_HELPER_PATH\" \"$@\"",
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-cleanroom-e2e.sh to contain %q", needle)
		}
	}

	for _, needle := range []string{
		"CLEANROOM_PRIVILEGED_MODE",
		"sudo -n \"$@\"",
	} {
		if strings.Contains(script, needle) {
			t.Fatalf("expected ci-cleanroom-e2e.sh not to contain %q", needle)
		}
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

func TestCiCleanroomE2EProbesHelperCapabilitiesInsteadOfHelperDrift(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-cleanroom-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-cleanroom-e2e.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		"ROOT_HELPER_REQUIRED_CAPABILITIES=(",
		"sudo -n \"$PRIVILEGED_HELPER_PATH\" capabilities",
		"verify_helper_capabilities",
		"Roll out the latest helper on the CI host",
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-cleanroom-e2e.sh to contain %q", needle)
		}
	}

	for _, needle := range []string{
		"ROOT_HELPER_LEGACY_CAPABILITIES=(",
		"unsupported command 'capabilities'",
		"assuming baseline helper capabilities",
		"sha256sum scripts/cleanroom-root-helper.sh",
		"Update with: sudo install -o root -g root -m 0755 scripts/cleanroom-root-helper.sh",
	} {
		if strings.Contains(script, needle) {
			t.Fatalf("expected ci-cleanroom-e2e.sh not to contain %q", needle)
		}
	}
}
