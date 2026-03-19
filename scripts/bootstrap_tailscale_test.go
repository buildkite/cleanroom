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
