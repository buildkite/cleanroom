package ci_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func requireContains(t *testing.T, path string, want string) {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if !strings.Contains(string(content), want) {
		t.Fatalf("expected %s to contain %q", path, want)
	}
}

func requireNotContains(t *testing.T, path string, want string) {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if strings.Contains(string(content), want) {
		t.Fatalf("expected %s not to contain %q", path, want)
	}
}

func TestLinuxCiDefaultsUseBootstrapScript(t *testing.T) {
	t.Helper()

	requireContains(t, "variables.tf", "default     = \"scripts/bootstrap-buildkite-agent.sh\"")
	requireContains(t, filepath.Join("..", "..", "modules", "linux-ci", "variables.tf"), "default     = \"scripts/bootstrap-buildkite-agent.sh\"")
	requireContains(t, "terraform.tfvars.example", "setup_script_path = \"scripts/bootstrap-buildkite-agent.sh\"")
}

func TestBootstrapScriptConfiguresBuildkiteAgent(t *testing.T) {
	t.Helper()

	scriptPath := filepath.Join("..", "..", "..", "..", "scripts", "bootstrap-buildkite-agent.sh")
	requireContains(t, scriptPath, "buildkite-agent start")
	requireContains(t, scriptPath, "BUILDKITE_TOKEN_PARAM")
}

func TestUserDataInstallsAwsCliWithoutAptAwscliDependency(t *testing.T) {
	t.Helper()

	templatePath := filepath.Join("..", "..", "modules", "linux-ci", "templates", "user_data.sh.tftpl")
	requireNotContains(t, templatePath, "apt-get install -y git jq curl tar ca-certificates openssh-client awscli")
	requireContains(t, templatePath, "awscli-exe-linux")
}

func TestLinuxCiEnablesNestedVirtualization(t *testing.T) {
	t.Helper()

	moduleMainPath := filepath.Join("..", "..", "modules", "linux-ci", "main.tf")
	requireContains(t, moduleMainPath, "nested_virtualization = \"enabled\"")
}
