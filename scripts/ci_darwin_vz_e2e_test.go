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
		`openssl x509 -inform der -in "$wwdr_path" -noout -fingerprint -sha1`,
		"provisioning profile does not allow this Mac's Provisioning UDID",
		`sudo security import "$p12_path" -k "$system_keychain_path" -P "$p12_password" -T /usr/bin/codesign -T /usr/bin/security >/dev/null`,
		`sudo security add-certificates -k "$system_keychain_path" "$wwdr_path" >/dev/null`,
		`security find-certificate -a -c "$requested_sign_identity" -Z "$system_keychain_path"`,
		`security find-identity -v -p codesigning "$system_keychain_path" 2>&1`,
		`sudo security delete-identity -Z "$imported_system_identity_hash" "$system_keychain_path" >/dev/null 2>&1 || true`,
		`sudo security delete-certificate -Z "$wwdr_fingerprint" "$system_keychain_path" >/dev/null 2>&1 || true`,
		`sign_identity="$requested_sign_identity"`,
		`sign_keychain="$system_keychain_path"`,
		"CLEANROOM_DARWIN_VZ_HELPER to a prebuilt signed helper bundle",
		"CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE and CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY",
		"AppleWWDRCAG3.cer",
		`requested_sign_identity`,
		`CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN="$sign_keychain"`,
		`imported signing identity not found in ${system_keychain_path}`,
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
