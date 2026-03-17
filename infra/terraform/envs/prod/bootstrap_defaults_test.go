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
	requireContains(t, "terraform.tfvars", "setup_script_path = \"scripts/bootstrap-cleanroom-host.sh\"")
}

func TestProdDefaultsUseLargeLinuxHost(t *testing.T) {
	t.Helper()

	requireContains(t, "variables.tf", "default     = \"m8i.4xlarge\"")
	requireContains(t, "variables.tf", "default     = 500")
	requireContains(t, "terraform.tfvars.example", "instance_type         = \"m8i.xlarge\"")
}

func TestProdBootstrapBuildsAndInstallsCleanroom(t *testing.T) {
	t.Helper()

	scriptPath := filepath.Join("..", "..", "..", "..", "scripts", "bootstrap-cleanroom-host.sh")
	requireContains(t, scriptPath, "scripts/build-go.sh")
	requireContains(t, scriptPath, "export GOPATH=\"${GOPATH:-$HOME/go}\"")
	requireContains(t, scriptPath, "export GOMODCACHE=\"${GOMODCACHE:-$GOPATH/pkg/mod}\"")
	requireContains(t, scriptPath, "daemon install --force --log-level info")
	requireContains(t, scriptPath, "default_backend: firecracker")
	requireContains(t, scriptPath, "memory_mib: ${CLEANROOM_FIRECRACKER_MEMORY_MIB}")
	requireContains(t, scriptPath, "install_tailscale_if_configured")
	requireContains(t, scriptPath, "CLEANROOM_TAILSCALE_AUTH_KEY_PARAMETER_NAME")
	requireContains(t, scriptPath, "Acquire::ForceIPv4 \"true\";")
}

func TestProdEnvSupportsAvailabilityZoneOverride(t *testing.T) {
	t.Helper()

	requireContains(t, "variables.tf", "variable \"availability_zone\"")
	requireContains(t, "main.tf", "availability_zone   = var.availability_zone")
	requireContains(t, "terraform.tfvars.example", "# availability_zone = \"ap-southeast-2b\"")
}

func TestVariableDefaultRegionIsUsWest2(t *testing.T) {
	t.Helper()

	block := readVariableBlock(t, "variables.tf", "aws_region")
	if !strings.Contains(block, "us-west-2") {
		t.Fatalf("expected default aws_region to be us-west-2 in variables.tf, got:\n%s", block)
	}
}

func TestExampleRegionIsSydney(t *testing.T) {
	t.Helper()

	requireContains(t, "terraform.tfvars.example", "aws_region  = \"ap-southeast-2\"")
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
	moduleUserDataPath := filepath.Join("..", "..", "modules", "linux-host", "templates", "user_data.sh.tftpl")
	requireContains(t, moduleVarsPath, "variable \"buildkite_token_parameter_name\"")
	requireContains(t, moduleVarsPath, "default     = \"\"")
	requireContains(t, moduleUserDataPath, "if [ -n \"$BUILDKITE_TOKEN_PARAM\" ]; then")
	requireContains(t, moduleUserDataPath, "warning: tailscale auth key parameter unavailable")
}
