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
		`normalize_secret_value()`,
		`resolve_macos_user_name()`,
		`resolve_macos_user_home()`,
		`run_with_macos_user_home()`,
		`resolve_macos_provisioning_udid()`,
		`assert_profile_allows_current_device()`,
		"fetch_secret CLEANROOM_DARWIN_VZ_HELPER_CERT_P12_BASE64",
		"fetch_secret CLEANROOM_DARWIN_VZ_HELPER_CERT_PASSWORD",
		"fetch_secret CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE_BASE64",
		"fetch_secret CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY",
		`tr -d '\r\n'`,
		`tr -d '\r'`,
		`stat -f '%Su' /dev/console`,
		`dscl . -read "/Users/${username}" NFSHomeDirectory`,
		`system_profiler SPHardwareDataType`,
		`openssl smime -inform der -verify -noverify -in "$profile_path" -out "$decoded_profile_path"`,
		"provisioning profile does not allow this Mac's Provisioning UDID",
		`awk '/^SHA-1 hash:/ {print $3; exit}'`,
		"CLEANROOM_DARWIN_VZ_HELPER to a prebuilt signed helper bundle",
		"CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE and CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY",
		"AppleWWDRCAG3.cer",
		`run_with_macos_user_home security add-certificates -k "$temp_keychain_path" "$wwdr_path" >/dev/null`,
		`run_with_macos_user_home security default-keychain -d user 2>/dev/null`,
		`requested_sign_identity`,
		`run_with_macos_user_home security find-certificate -a -c "$requested_sign_identity" "$temp_keychain_path"`,
		`sign_identity="$imported_sign_identity"`,
		`run_with_macos_user_home security find-identity -v -p codesigning 2>&1`,
		`CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN="$sign_keychain"`,
		"warning: imported signing identity not found in configured keychain search list",
		"warning: continuing to codesign with the temp keychain; codesign will be the source of truth",
		`run_with_macos_user_home env`,
		`run_with_macos_user_home codesign --verify --strict --verbose=2 "$helper_path"`,
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

func TestBuildDarwinVZHelperSupportsExplicitSigningKeychain(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("build-darwin-vz-helper.sh")
	if err != nil {
		t.Fatalf("read build-darwin-vz-helper.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		"CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN",
		`codesign_args+=(--keychain "${SIGN_KEYCHAIN}")`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected build-darwin-vz-helper.sh to contain %q", needle)
		}
	}
}
