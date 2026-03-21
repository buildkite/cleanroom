package prod_test

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

func TestProdDefaultsUseCleanroomBootstrapScript(t *testing.T) {
	t.Helper()

	requireContains(t, "variables.tf", "default     = \"scripts/bootstrap-cleanroom-host.sh\"")
	requireContains(t, "terraform.tfvars.example", "setup_script_path = \"scripts/bootstrap-cleanroom-host.sh\"")
	requireContains(t, "prod.ap-southeast-2.tfvars", "setup_script_path            = \"scripts/bootstrap-cleanroom-host.sh\"")
	requireContains(t, "prod.us-west-2.tfvars", "setup_script_path            = \"scripts/bootstrap-cleanroom-host.sh\"")
}

func TestProdDefaultsUseLargeLinuxHost(t *testing.T) {
	t.Helper()

	requireContains(t, "variables.tf", "default     = \"m8i.4xlarge\"")
	requireContains(t, "variables.tf", "default     = 500")
	requireContains(t, "terraform.tfvars.example", "instance_type        = \"m8i.xlarge\"")
}

func TestProdBootstrapInstallsPinnedReleaseAndBootstrapRunner(t *testing.T) {
	t.Helper()

	scriptPath := filepath.Join("..", "..", "..", "..", "scripts", "bootstrap-cleanroom-host.sh")
	requireContains(t, scriptPath, "installing cleanroom release")
	requireContains(t, scriptPath, "cleanroom-root-helper")
	requireContains(t, scriptPath, "retry 5 5 curl -fsSL \"$install_script_url\" -o \"$install_script_path\"")
	requireContains(t, scriptPath, "retry 5 5 env \\")
	requireContains(t, scriptPath, "privileged_helper_path: ${HELPER_INSTALL_PATH}")
	requireContains(t, scriptPath, "BOOTSTRAP_ENV_PATH='/usr/local/etc/cleanroom-bootstrap-host.env'")
	requireContains(t, scriptPath, "BOOTSTRAP_RUNNER_PATH='/usr/local/bin/cleanroom-bootstrap-host'")
	requireContains(t, scriptPath, "CLEANROOM_VERSION")
	requireContains(t, scriptPath, "CLEANROOM_INSTALL_SCRIPT_REF")
	requireContains(t, scriptPath, "CLEANROOM_RELEASE_REPO")
	requireContains(t, scriptPath, "CLEANROOM_RELEASE_REPO=''")
	requireNotContains(t, scriptPath, "match($0, /^[[:space:]]*[A-Za-z0-9_]+[[:space:]]*=[[:space:]]*\"([^\"]*)\"/, m)")
	requireNotContains(t, scriptPath, "match($0, /^[[:space:]]*[A-Za-z0-9_]+[[:space:]]*=[[:space:]]*(true|false)/, m)")
	requireContains(t, scriptPath, "apply_prod_tfvars_overrides \"$repo_root\"")
	requireContains(t, scriptPath, "prod.${AWS_REGION}.tfvars")
	requireContains(t, scriptPath, "fetch_repo_checkout \"$bootstrap_repo_url\" \"$bootstrap_repo_ref\"")
	requireContains(t, scriptPath, "if [ \"$REPO_URL\" != \"$bootstrap_repo_url\" ] || [ \"$REPO_REF\" != \"$bootstrap_repo_ref\" ]; then")
	requireNotContains(t, scriptPath, "CLEANROOM_REPO='$RELEASE_REPO'")
	requireNotContains(t, scriptPath, "export CLEANROOM_REPO=\"$CLEANROOM_REPO\"")
	requireNotContains(t, scriptPath, "RELEASE_REPO=\"buildkite/cleanroom\"")
	requireContains(t, scriptPath, "daemon install --force --log-level info")
	requireContains(t, scriptPath, "default_backend: firecracker")
	requireContains(t, scriptPath, "memory_mib: ${CLEANROOM_FIRECRACKER_MEMORY_MIB}")
	requireContains(t, scriptPath, "install_tailscale_if_configured")
	requireContains(t, scriptPath, "Acquire::ForceIPv4 \"true\";")
	requireNotContains(t, scriptPath, "curl -fsSL \"\\$1\" | bash")
	requireNotContains(t, scriptPath, "scripts/build-go.sh")
}

func TestProdEnvSupportsAvailabilityZoneOverride(t *testing.T) {
	t.Helper()

	requireContains(t, "variables.tf", "variable \"availability_zone\"")
	requireContains(t, "main.tf", "availability_zone   = var.availability_zone")
	requireContains(t, "terraform.tfvars.example", "availability_zone = \"ap-southeast-2b\"")
}

func TestAwsRegionMustBeProvidedExplicitly(t *testing.T) {
	t.Helper()

	block := readVariableBlock(t, "variables.tf", "aws_region")
	if strings.Contains(block, "default") {
		t.Fatalf("expected aws_region to have no default in variables.tf")
	}
	requireContains(t, "README.md", "-var-file=prod.ap-southeast-2.tfvars")
	requireContains(t, "README.md", "terraform workspace select -or-create ap-southeast-2")
	requireContains(t, "terraform.tfvars", "terraform workspace select -or-create ap-southeast-2")
}

func TestGitDeployKeyIsRequired(t *testing.T) {
	t.Helper()

	block := readVariableBlock(t, "variables.tf", "git_deploy_key_parameter_name")
	if strings.Contains(block, "default") {
		t.Fatalf("expected git_deploy_key_parameter_name to have no default in variables.tf")
	}

	if !strings.Contains(block, "trimspace(var.git_deploy_key_parameter_name) != \"\"") {
		t.Fatalf("expected git_deploy_key_parameter_name validation in variables.tf")
	}
}

func TestSharedLinuxModuleSupportsNonCiBootstrap(t *testing.T) {
	t.Helper()

	moduleVarsPath := filepath.Join("..", "..", "modules", "linux-host", "variables.tf")
	moduleMainPath := filepath.Join("..", "..", "modules", "linux-host", "main.tf")
	moduleUserDataPath := filepath.Join("..", "..", "modules", "linux-host", "templates", "user_data.sh.tftpl")
	requireContains(t, moduleVarsPath, "variable \"buildkite_token_parameter_name\"")
	requireContains(t, moduleVarsPath, "default     = \"\"")
	requireContains(t, moduleVarsPath, "variable \"user_data_replace_on_change\"")
	requireContains(t, moduleVarsPath, "default     = true")
	requireContains(t, moduleVarsPath, "variable \"cleanroom_version\"")
	requireContains(t, moduleVarsPath, "default     = \"v0.3.0\"")
	requireContains(t, moduleVarsPath, "variable \"cleanroom_install_script_ref\"")
	requireContains(t, moduleVarsPath, "variable \"cleanroom_release_repo\"")
	requireContains(t, moduleMainPath, "user_data_replace_on_change = var.user_data_replace_on_change")
	requireContains(t, "main.tf", "user_data_replace_on_change       = false")
	requireNotContains(t, moduleMainPath, "ignore_changes = [user_data]")
	requireContains(t, moduleUserDataPath, "Acquire::ForceIPv4 \"true\";")
	requireContains(t, moduleUserDataPath, "export CLEANROOM_VERSION=\"$CLEANROOM_VERSION\"")
	requireContains(t, moduleUserDataPath, "export CLEANROOM_INSTALL_SCRIPT_REF=\"$CLEANROOM_INSTALL_SCRIPT_REF\"")
	requireContains(t, moduleUserDataPath, "export CLEANROOM_RELEASE_REPO=\"$CLEANROOM_RELEASE_REPO\"")
	requireContains(t, moduleUserDataPath, "if [ -n \"$BUILDKITE_TOKEN_PARAM\" ]; then")
	requireContains(t, moduleUserDataPath, "warning: tailscale auth key parameter unavailable")
}
