package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestCIBootstrapLinuxSSMScriptSupportsRunAndLogs(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-bootstrap-linux-ssm.sh")
	if err != nil {
		t.Fatalf("read ci-bootstrap-linux-ssm.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		"usage: scripts/ci-bootstrap-linux-ssm.sh <run|logs>",
		"CLEANROOM_CI_AWS_PROFILE",
		"CLEANROOM_CI_AWS_REGION",
		"CLEANROOM_CI_INSTANCE_ID",
		"CLEANROOM_CI_TERRAFORM_DIR",
		"DEFAULT_CI_AWS_REGION=\"ap-southeast-2\"",
		"terraform -chdir=\"$terraform_dir\" output -raw instance_id",
		"aws ssm send-command",
		"aws ssm wait command-executed",
		"aws ssm get-command-invocation",
		"run)",
		"logs)",
		"/usr/local/bin/cleanroom-bootstrap-linux",
		"/var/log/cleanroom-bootstrap-linux.log",
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-bootstrap-linux-ssm.sh to contain %q", needle)
		}
	}
}
