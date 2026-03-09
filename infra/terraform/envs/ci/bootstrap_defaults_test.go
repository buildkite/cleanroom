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

func readVariableBlock(t *testing.T, path string, variableName string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	marker := "variable \"" + variableName + "\" {"
	src := string(content)
	start := strings.Index(src, marker)
	if start == -1 {
		t.Fatalf("could not find variable %q in %s", variableName, path)
	}

	braceDepth := 0
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '{':
			braceDepth++
		case '}':
			braceDepth--
			if braceDepth == 0 {
				return src[start : i+1]
			}
		}
	}

	t.Fatalf("unterminated variable block for %q in %s", variableName, path)
	return ""
}

func TestLinuxCiDefaultsUseBootstrapScript(t *testing.T) {
	t.Helper()

	requireContains(t, "variables.tf", "default     = \"scripts/bootstrap-buildkite-agent.sh\"")
	requireContains(t, filepath.Join("..", "..", "modules", "linux-ci", "variables.tf"), "default     = \"scripts/bootstrap-buildkite-agent.sh\"")
	requireContains(t, "terraform.tfvars.example", "setup_script_path = \"scripts/bootstrap-buildkite-agent.sh\"")
}

func TestMacCiDefaultsUseBootstrapScript(t *testing.T) {
	t.Helper()

	requireContains(t, "variables.tf", "default     = \"scripts/bootstrap-buildkite-agent-macos.sh\"")
	requireContains(t, "variables.tf", "default     = \"cleanroom-mac\"")
	requireContains(t, filepath.Join("..", "..", "modules", "macos-ci", "variables.tf"), "default     = \"scripts/bootstrap-buildkite-agent-macos.sh\"")
	requireContains(t, filepath.Join("..", "..", "modules", "macos-ci", "variables.tf"), "default     = \"cleanroom-mac\"")
	requireContains(t, "terraform.tfvars.example", "mac_setup_script_path = \"scripts/bootstrap-buildkite-agent-macos.sh\"")
	requireContains(t, "terraform.tfvars.example", "mac_buildkite_queue   = \"cleanroom-mac\"")
}

func TestBootstrapScriptConfiguresBuildkiteAgent(t *testing.T) {
	t.Helper()

	scriptPath := filepath.Join("..", "..", "..", "..", "scripts", "bootstrap-buildkite-agent.sh")
	requireContains(t, scriptPath, "buildkite-agent start")
	requireContains(t, scriptPath, "BUILDKITE_TOKEN_PARAM")
}

func TestMacBootstrapScriptConfiguresBuildkiteAgent(t *testing.T) {
	t.Helper()

	scriptPath := filepath.Join("..", "..", "..", "..", "scripts", "bootstrap-buildkite-agent-macos.sh")
	requireContains(t, scriptPath, "<string>start</string>")
	requireContains(t, scriptPath, "CLEANROOM_BUILDKITE_QUEUE")
	requireContains(t, scriptPath, "QUEUE_NAME=\"${CLEANROOM_BUILDKITE_QUEUE:-cleanroom-mac}\"")
	requireContains(t, scriptPath, "BUILD_PATH=\"/buildkite/builds\"")
	requireContains(t, scriptPath, "<string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>")
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

func TestMacCiUsesDedicatedHostAndPrivateNetworking(t *testing.T) {
	t.Helper()

	moduleMainPath := filepath.Join("..", "..", "modules", "macos-ci", "main.tf")
	requireContains(t, moduleMainPath, "resource \"aws_ec2_host\" \"mac\"")
	requireContains(t, moduleMainPath, "tenancy                = \"host\"")
	requireContains(t, moduleMainPath, "associate_public_ip_address = false")
	requireContains(t, moduleMainPath, "aws_iam_role_policy.parameter_read")
}

func TestEnvWiresOptionalMacCiModule(t *testing.T) {
	t.Helper()

	requireContains(t, "main.tf", "module \"mac_ci\"")
	requireContains(t, "main.tf", "count  = var.enable_macos_ci ? 1 : 0")
}

func TestGitDeployKeyIsRequired(t *testing.T) {
	t.Helper()

	files := []string{
		"variables.tf",
		filepath.Join("..", "..", "modules", "linux-ci", "variables.tf"),
		filepath.Join("..", "..", "modules", "macos-ci", "variables.tf"),
	}

	for _, path := range files {
		block := readVariableBlock(t, path, "git_deploy_key_parameter_name")

		if strings.Contains(block, "default") {
			t.Fatalf("expected git_deploy_key_parameter_name to have no default in %s", path)
		}

		if !strings.Contains(block, "trimspace(var.git_deploy_key_parameter_name) != \"\"") {
			t.Fatalf("expected git_deploy_key_parameter_name validation in %s", path)
		}
	}
}
