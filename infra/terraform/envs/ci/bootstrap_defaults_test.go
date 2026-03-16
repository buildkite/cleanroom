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
	requireContains(t, filepath.Join("..", "..", "modules", "linux-host", "variables.tf"), "default     = \"scripts/bootstrap-buildkite-agent.sh\"")
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
	requireContains(t, scriptPath, "BUILD_PATH=\"${CLEANROOM_BUILDKITE_BUILD_PATH:-${AGENT_ROOT}/builds}\"")
	requireNotContains(t, scriptPath, "install -d -o \"$AGENT_USER\" -g \"$AGENT_GROUP\" -m 0755 /buildkite")
	requireContains(t, scriptPath, "install e2fsprogs")
	requireContains(t, scriptPath, "https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh")
	requireContains(t, scriptPath, "AGENT_SERVICE_PATH=\"/opt/homebrew/opt/e2fsprogs/sbin:/opt/homebrew/opt/e2fsprogs/bin:/usr/local/opt/e2fsprogs/sbin:/usr/local/opt/e2fsprogs/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin\"")
	requireContains(t, scriptPath, "<string>${AGENT_SERVICE_PATH}</string>")
	requireContains(t, scriptPath, "<key>UserName</key>")
}

func TestUserDataInstallsAwsCliWithoutAptAwscliDependency(t *testing.T) {
	t.Helper()

	templatePath := filepath.Join("..", "..", "modules", "linux-host", "templates", "user_data.sh.tftpl")
	requireNotContains(t, templatePath, "apt-get install -y git jq curl tar ca-certificates openssh-client awscli")
	requireContains(t, templatePath, "awscli-exe-linux")
}

func TestUserDataIsUbuntuSpecific(t *testing.T) {
	t.Helper()

	templatePath := filepath.Join("..", "..", "modules", "linux-host", "templates", "user_data.sh.tftpl")
	requireContains(t, templatePath, "linux host user_data requires an Ubuntu apt-based AMI")
	requireNotContains(t, templatePath, "if command -v dnf >/dev/null 2>&1; then")
	requireNotContains(t, templatePath, "yum install -y")
}

func TestUserDataVerifiesZfsAvailability(t *testing.T) {
	t.Helper()

	templatePath := filepath.Join("..", "..", "modules", "linux-host", "templates", "user_data.sh.tftpl")
	requireContains(t, templatePath, "linux-headers-$(uname -r)")
	requireContains(t, templatePath, "modprobe zfs")
	requireContains(t, templatePath, "command -v zpool >/dev/null 2>&1")
}

func TestUserDataCreatesZfsPoolFromEphemeralNVMe(t *testing.T) {
	t.Helper()

	templatePath := filepath.Join("..", "..", "modules", "linux-host", "templates", "user_data.sh.tftpl")
	requireContains(t, templatePath, "zpool create -f")
	requireContains(t, templatePath, "Amazon Elastic Block Store")
	requireContains(t, templatePath, "zfs create")
	requireContains(t, templatePath, "cleanroom-zfs.img")
	requireContains(t, templatePath, "CLEANROOM_ZFS_LOOPBACK_SIZE")
	requireContains(t, templatePath, "truncate -s \"$CLEANROOM_ZFS_LOOPBACK_SIZE\"")
}

func TestDefaultRegionIsUsWest2(t *testing.T) {
	t.Helper()

	for _, path := range []string{
		"variables.tf",
		filepath.Join("..", "..", "modules", "linux-host", "variables.tf"),
		filepath.Join("..", "..", "modules", "macos-ci", "variables.tf"),
	} {
		block := readVariableBlock(t, path, "aws_region")
		if !strings.Contains(block, "us-west-2") {
			t.Fatalf("expected default aws_region to be us-west-2 in %s, got:\n%s", path, block)
		}
	}

	requireContains(t, "terraform.tfvars.example", "aws_region  = \"us-west-2\"")
}

func TestBootstrapConfiguresZfsSnapshots(t *testing.T) {
	t.Helper()

	scriptPath := filepath.Join("..", "..", "..", "..", "scripts", "bootstrap-buildkite-agent.sh")
	requireContains(t, scriptPath, "CLEANROOM_ZFS_DATASET")
	requireContains(t, scriptPath, "driver: file")
	requireNotContains(t, scriptPath, "driver: zfs")
	requireContains(t, scriptPath, "snapshots:")
	requireContains(t, scriptPath, "enabled: true")
}

func TestLinuxCiEnablesNestedVirtualization(t *testing.T) {
	t.Helper()

	moduleMainPath := filepath.Join("..", "..", "modules", "linux-host", "main.tf")
	requireContains(t, moduleMainPath, "nested_virtualization = \"enabled\"")
}

func TestMacCiUsesDedicatedHostAndPrivateNetworking(t *testing.T) {
	t.Helper()

	moduleMainPath := filepath.Join("..", "..", "modules", "macos-ci", "main.tf")
	requireContains(t, moduleMainPath, "resource \"aws_ec2_host\" \"mac\"")
	requireContains(t, moduleMainPath, "prevent_destroy = true")
	requireContains(t, moduleMainPath, "tenancy                = \"host\"")
	requireContains(t, moduleMainPath, "associate_public_ip_address = false")
	requireNotContains(t, moduleMainPath, "user_data_replace_on_change = true")
	requireContains(t, moduleMainPath, "ignore_changes = [user_data]")
	requireContains(t, moduleMainPath, "aws_iam_role_policy.parameter_read")
}

func TestMacUserDataSupportsInPlaceBootstrapRerun(t *testing.T) {
	t.Helper()

	templatePath := filepath.Join("..", "..", "modules", "macos-ci", "templates", "user_data.sh.tftpl")
	requireContains(t, templatePath, "BOOTSTRAP_ENV_PATH='/usr/local/etc/cleanroom-bootstrap-macos.env'")
	requireContains(t, templatePath, "BOOTSTRAP_RUNNER_PATH='/usr/local/bin/cleanroom-bootstrap-macos'")
	requireContains(t, templatePath, "$${AWS_REGION:?AWS_REGION must be set}")
	requireContains(t, templatePath, "PATH=\"/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$${PATH:-}\"")
	requireContains(t, templatePath, "source \"$BOOTSTRAP_ENV_PATH\"")
	requireContains(t, templatePath, "chmod 0755 \"$BOOTSTRAP_RUNNER_PATH\"")
	requireContains(t, templatePath, "\"$BOOTSTRAP_RUNNER_PATH\"")
}

func TestEnvWiresOptionalMacCiModule(t *testing.T) {
	t.Helper()

	requireContains(t, "main.tf", "module \"mac_ci\"")
	requireContains(t, "main.tf", "count  = var.enable_macos_ci ? 1 : 0")
}

func TestEnvSupportsAvailabilityZoneOverride(t *testing.T) {
	t.Helper()

	requireContains(t, "variables.tf", "variable \"availability_zone\"")
	requireContains(t, "main.tf", "availability_zone   = var.availability_zone")
	requireContains(t, "terraform.tfvars.example", "# availability_zone = \"us-west-2b\"")
}

func TestGitDeployKeyIsRequired(t *testing.T) {
	t.Helper()

	files := []string{
		"variables.tf",
		filepath.Join("..", "..", "modules", "linux-host", "variables.tf"),
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
