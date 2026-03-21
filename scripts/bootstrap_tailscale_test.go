package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestBootstrapScriptsSkipTailscaleWhenAuthKeyLookupFails(t *testing.T) {
	t.Helper()

	for _, tc := range []struct {
		name string
		path string
	}{
		{
			name: "buildkite-agent",
			path: "bootstrap-buildkite-agent.sh",
		},
		{
			name: "cleanroom-host",
			path: "bootstrap-cleanroom-host.sh",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("read %s: %v", tc.path, err)
			}

			script := string(content)
			needle := `if ! tailscale_auth_key="$(retry 10 3 aws ssm get-parameter --region "$AWS_REGION" --name "$tailscale_param" --with-decryption --query 'Parameter.Value' --output text)"; then
    warn "tailscale auth key parameter unavailable (${tailscale_param}); skipping tailscale bootstrap"
    return
  fi

  log "installing tailscale ${tailscale_version}"`

			if !strings.Contains(script, needle) {
				t.Fatalf("expected %s to skip tailscale bootstrap when auth key lookup fails", tc.path)
			}
		})
	}
}

func TestBootstrapBuildkiteAgentCreatesHelperParentDir(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("bootstrap-buildkite-agent.sh")
	if err != nil {
		t.Fatalf("read bootstrap-buildkite-agent.sh: %v", err)
	}

	script := string(content)
	needle := "install -d -o root -g root -m 0755 \"$(dirname \"$HELPER_INSTALL_PATH\")\"\ninstall -o root -g root -m 0755 \"$HELPER_SOURCE_PATH\" \"$HELPER_INSTALL_PATH\""
	if !strings.Contains(script, needle) {
		t.Fatal("expected bootstrap-buildkite-agent.sh to create the helper parent directory before installing the helper")
	}
}

func TestBootstrapCleanroomHostRejectsLegacyBinaryDirWithoutBinSuffix(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("bootstrap-cleanroom-host.sh")
	if err != nil {
		t.Fatalf("read bootstrap-cleanroom-host.sh: %v", err)
	}

	script := string(content)
	needle := "*) die \"CLEANROOM_BINARY_INSTALL_DIR must end with /bin when CLEANROOM_INSTALL_PREFIX is not set\" ;;"
	if !strings.Contains(script, needle) {
		t.Fatal("expected bootstrap-cleanroom-host.sh to reject CLEANROOM_BINARY_INSTALL_DIR values that do not end with /bin when inferring the install prefix")
	}
}
