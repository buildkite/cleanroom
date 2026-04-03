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
	if !strings.Contains(string(content), `helper_path="${CLEANROOM_DARWIN_VZ_HELPER:-$PWD/dist/cleanroom-darwin-vz.app}"`) {
		t.Fatalf("expected ci-darwin-vz-e2e.sh to default to the prebuilt helper app bundle")
	}
}

func TestBuildDarwinVZHelperUsesSharedPackager(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("build-darwin-vz-helper.sh")
	if err != nil {
		t.Fatalf("read build-darwin-vz-helper.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		`tmpdir="$(mktemp -d /tmp/cleanroom-darwin-vz-build.XXXXXX)"`,
		`build_output_path="${tmpdir}/cleanroom-darwin-vz"`,
		`BUNDLE_MODE="${CLEANROOM_DARWIN_VZ_HELPER_BUNDLE:-1}"`,
		`"CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS=${ENTITLEMENTS_PATH}"`,
		`"CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY=${CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTITY:--}"`,
		`"CLEANROOM_DARWIN_VZ_HELPER_SIGN_RUNTIME=${SIGN_RUNTIME}"`,
		`"${BUNDLE_MODE}" != "0"`,
		`env "${package_env[@]}" "${SCRIPT_DIR}/package-darwin-vz-helper.sh" "${build_output_path}" "${OUTPUT_PATH}"`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected build-darwin-vz-helper.sh to contain %q", needle)
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
		`setup_buildkite_runtime_paths()`,
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
		`xdg_state_home_path="/tmp/cleanroom-state-$(openssl rand -hex 4)"`,
		`XDG_STATE_HOME="$xdg_state_home_path"`,
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
		`scripts/build-darwin-vz-helper.sh dist/cleanroom-darwin-vz.app`,
		"CLEANROOM_DARWIN_VZ_VMNET_E2E=1",
		`go test ./internal/backend/darwinvz -run TestVMNetSharedE2E -v`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-darwin-vz-vmnet-e2e.sh to contain %q", needle)
		}
	}
}

func TestPackageDarwinVZHelperSupportsExplicitSigningKeychain(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("package-darwin-vz-helper.sh")
	if err != nil {
		t.Fatalf("read package-darwin-vz-helper.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		"CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN",
		"CLEANROOM_DARWIN_VZ_HELPER_SIGN_RUNTIME",
		`args+=(--keychain "${SIGN_KEYCHAIN}")`,
		`args+=(--options runtime --timestamp)`,
		`else`,
		`rm -f "${PROFILE_DEST}"`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected package-darwin-vz-helper.sh to contain %q", needle)
		}
	}
}

func TestBuildMacOSReleasePkgSupportsHelperBundle(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("build-macos-release-pkg.sh")
	if err != nil {
		t.Fatalf("read build-macos-release-pkg.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		`Path to the cleanroom-darwin-vz macOS binary or .app bundle`,
		`[[ -e "${HELPER_BINARY}" ]] || die "missing cleanroom-darwin-vz helper: ${HELPER_BINARY}"`,
		`if [[ -d "${HELPER_BINARY}" ]]; then`,
		`PAYLOAD_HELPER_PATH="${PAYLOAD_BIN_DIR}/cleanroom-darwin-vz.app"`,
		`ditto "${HELPER_BINARY}" "${PAYLOAD_HELPER_PATH}"`,
		`configure_helper_component_plist`,
		`configure_helper_cleanup_scripts`,
		`legacy_helper_path="${INSTALL_PREFIX}/cleanroom-darwin-vz"`,
		`pkgbuild_args+=(--scripts "${helper_cleanup_scripts_dir}")`,
		`pkgbuild --analyze --root "${PAYLOAD_ROOT}" "${helper_component_plist}" >/dev/null`,
		`pkgbuild_args+=(--component-plist "${helper_component_plist}")`,
		`codesign --verify --strict --verbose=2 "${PAYLOAD_HELPER_PATH}"`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected build-macos-release-pkg.sh to contain %q", needle)
		}
	}
}

func TestNotarizeMacOSPackageSupportsKeychainProfile(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("notarize-macos-package.sh")
	if err != nil {
		t.Fatalf("read notarize-macos-package.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		"CLEANROOM_MACOS_NOTARY_KEYCHAIN_PROFILE",
		"CLEANROOM_MACOS_NOTARY_KEYCHAIN_PATH",
		`notarytool_args+=(--keychain-profile "${KEYCHAIN_PROFILE}")`,
		`notarytool_args+=(--keychain "${KEYCHAIN_PATH}")`,
		`xcrun notarytool "${notarytool_args[@]}"`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected notarize-macos-package.sh to contain %q", needle)
		}
	}
}

func TestReleaseWorkflowBuildsVMNetSignedNotarizedPackages(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("../.github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}

	workflow := string(content)
	for _, needle := range []string{
		`tags:`,
		`- "v*"`,
		`release_ref_name="${GITHUB_REF_NAME}"`,
		`release_version="${GITHUB_REF_NAME#v}"`,
		`echo "CLEANROOM_RELEASE_REF_NAME=${release_ref_name}" >> "$GITHUB_ENV"`,
		`echo "CLEANROOM_RELEASE_VERSION=${release_version}" >> "$GITHUB_ENV"`,
		`CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE_BASE64`,
		`apple-actions/import-codesign-certs@v3`,
		`xcrun notarytool store-credentials "${CLEANROOM_NOTARY_PROFILE}"`,
		`CLEANROOM_DARWIN_VZ_HELPER_ENTITLEMENTS="cmd/cleanroom-darwin-vz/entitlements-vmnet.plist"`,
		`CLEANROOM_DARWIN_VZ_HELPER_SIGN_RUNTIME=1`,
		`CLEANROOM_DARWIN_VZ_HELPER_BUNDLE=1`,
		`main.version=${CLEANROOM_RELEASE_REF_NAME}`,
		`CLEANROOM_MACOS_RELEASE_VERSION="${CLEANROOM_RELEASE_VERSION}"`,
		`CLEANROOM_MACOS_RELEASE_HELPER_BINARY="release-extra/darwin_${{ matrix.arch }}/cleanroom-darwin-vz.app"`,
		`CLEANROOM_MACOS_NOTARY_KEYCHAIN_PROFILE="${CLEANROOM_NOTARY_PROFILE}"`,
		`if: github.event_name == 'push' && github.ref_type == 'tag'`,
		`gh release upload "${GITHUB_REF_NAME}"`,
	} {
		if !strings.Contains(workflow, needle) {
			t.Fatalf("expected release workflow to contain %q", needle)
		}
	}
}

func TestBuildkiteMacOSReleasePkgScriptBuildsNotarizedArtifacts(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-macos-release-pkg.sh")
	if err != nil {
		t.Fatalf("read ci-macos-release-pkg.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		`buildkite-agent secret get "$key"`,
		`CLEANROOM_MACOS_RELEASE_HELPER_CERT_P12_BASE64`,
		`CLEANROOM_MACOS_RELEASE_HELPER_CERT_PASSWORD`,
		`CLEANROOM_MACOS_RELEASE_HELPER_PROVISION_PROFILE_BASE64`,
		`CLEANROOM_MACOS_RELEASE_HELPER_SIGN_IDENTITY`,
		`CLEANROOM_MACOS_RELEASE_INSTALLER_CERT_P12_BASE64`,
		`CLEANROOM_MACOS_RELEASE_INSTALLER_CERT_PASSWORD`,
		`CLEANROOM_MACOS_INSTALLER_SIGN_IDENTITY`,
		`CLEANROOM_MACOS_NOTARY_KEY_P8_BASE64`,
		`system_keychain_path="/Library/Keychains/System.keychain"`,
		`sudo security import "${p12_path}"`,
		`installer_p12_path="${tmpdir}/installer-cert.p12"`,
		`installer_keychain_path="${tmpdir}/installer-signing.keychain-db"`,
		`security create-keychain -p "${installer_keychain_password}" "${installer_keychain_path}" >/dev/null`,
		`AppleIncRootCertificate.cer`,
		`DeveloperIDG2CA.cer`,
		`security import "${installer_p12_path}"`,
		`-T /usr/bin/productsign`,
		`security set-key-partition-list`,
		`keychain_path="${system_keychain_path}"`,
		`CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN="${keychain_path}"`,
		`CLEANROOM_MACOS_RELEASE_INSTALLER_SIGN_IDENTITY="${installer_sign_identity}"`,
		`CLEANROOM_MACOS_RELEASE_INSTALLER_SIGN_KEYCHAIN="${installer_keychain_path}"`,
		`build_release_arch arm64 arm64 arm64 arm64-apple-macosx13.0`,
		`build_release_arch amd64 x86_64 amd64 x86_64-apple-macosx13.0`,
		`buildkite-agent artifact upload "release-extra/darwin_*/*.pkg"`,
		`buildkite-agent artifact upload "release-extra/darwin_*/*.pkg.sha256"`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-macos-release-pkg.sh to contain %q", needle)
		}
	}
}

func TestInstallScriptUsesSharedPackagerWhenAvailable(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		`package_darwin_helper_with_repo_script()`,
		`package_script="${SCRIPT_DIR}/package-darwin-vz-helper.sh"`,
		`CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN Optional keychain path when using the repo helper packager`,
		`HELPER_BUNDLE_EMBEDDED_PROFILE_PATH="${HELPER_BUNDLE_SRC}/Contents/embedded.provisionprofile"`,
		`[ "${HELPER_RESIGN_REQUESTED}" = "0" ] && [ -f "${HELPER_BUNDLE_EMBEDDED_PROFILE_PATH}" ]`,
		`if ! package_darwin_helper_with_repo_script "${HELPER_BUNDLE_DIR}" "${HELPER_BUNDLE_DIR}"; then`,
		`HELPER_BUNDLE_PROFILE_DEST="${HELPER_BUNDLE_DIR}/Contents/embedded.provisionprofile"`,
		`"${SUDO_CMD[@]}" rm -f "${HELPER_BUNDLE_PROFILE_DEST}"`,
		`if ! package_darwin_helper_with_repo_script "${HELPER_BINARY_SRC}" "${HELPER_SIGN_TARGET}"; then`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected install.sh to contain %q", needle)
		}
	}
}
