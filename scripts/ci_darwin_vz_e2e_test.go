package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestCiDarwinVZE2EBootstrapsLinuxGuestAgentBinary(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-darwin-vz-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-darwin-vz-e2e.sh: %v", err)
	}

	if !strings.Contains(string(content), "GOOS=linux GOARCH=\"$host_arch\" CGO_ENABLED=0 go build -trimpath -o \"dist/cleanroom-guest-agent-linux-$host_arch\" ./cmd/cleanroom-guest-agent") {
		t.Fatalf("expected ci-darwin-vz-e2e.sh to bootstrap cleanroom-guest-agent-linux-$host_arch")
	}
}

func TestCiDarwinVZE2EForcesNATNetworkMode(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-darwin-vz-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-darwin-vz-e2e.sh: %v", err)
	}

	if !strings.Contains(string(content), "mode: nat") {
		t.Fatalf("expected ci-darwin-vz-e2e.sh to pin darwin-vz CI to nat mode")
	}
}

func TestSyncDarwinVZVmnetBuildkiteSecretsUsesParameterizedInputs(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("sync-darwin-vz-vmnet-buildkite-secrets.sh")
	if err != nil {
		t.Fatalf("read sync-darwin-vz-vmnet-buildkite-secrets.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		"BUILDKITE_CLUSTER_ID",
		"BK_CLUSTER_ID",
		"CLEANROOM_DARWIN_VZ_HELPER_CERT_P12_PATH",
		"CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE_PATH",
		"CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY",
		"CLEANROOM_DARWIN_VZ_HELPER_CERT_PASSWORD",
		"CLEANROOM_DARWIN_VZ_HELPER_CERT_P12_BASE64",
		"CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE_BASE64",
		"bk secret delete",
		"bk secret create",
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected sync-darwin-vz-vmnet-buildkite-secrets.sh to contain %q", needle)
		}
	}

	for _, needle := range []string{
		"--organization",
		"ORG_SLUG",
	} {
		if strings.Contains(script, needle) {
			t.Fatalf("expected sync-darwin-vz-vmnet-buildkite-secrets.sh not to contain %q", needle)
		}
	}
}

func TestCiDarwinVZVMNetE2EUsesBuildkiteSecretsAndVMNetEntitlements(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-darwin-vz-vmnet-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-darwin-vz-vmnet-e2e.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		`if [[ -z "${BUILDKITE:-}" ]]; then`,
		`buildkite-agent secret get "$key"`,
		"fetch_secret CLEANROOM_DARWIN_VZ_HELPER_CERT_P12_BASE64",
		"fetch_secret CLEANROOM_DARWIN_VZ_HELPER_CERT_PASSWORD",
		"fetch_secret CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE_BASE64",
		"fetch_secret CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY",
		"CLEANROOM_DARWIN_VZ_HELPER to a prebuilt signed helper bundle",
		"CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE and CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY",
		"AppleWWDRCAG3.cer",
		"security default-keychain -d user 2>/dev/null",
		"CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS=cmd/cleanroom-darwin-vz/entitlements-vmnet.plist",
		"CLEANROOM_DARWIN_VZ_HELPER_BUNDLE=1",
		"CLEANROOM_DARWIN_VZ_VMNET_E2E=1",
		`go test ./internal/backend/darwinvz -run TestVMNetSharedE2E -v`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-darwin-vz-vmnet-e2e.sh to contain %q", needle)
		}
	}
}
