package scripts_test

import (
	"errors"
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

func TestCiDarwinVZE2EUsesFileHandleNetworkMode(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-darwin-vz-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-darwin-vz-e2e.sh: %v", err)
	}

	if !strings.Contains(string(content), "mode: filehandle") {
		t.Fatalf("expected ci-darwin-vz-e2e.sh to pin darwin-vz CI to filehandle mode")
	}
	if !strings.Contains(string(content), `helper_path="${CLEANROOM_DARWIN_VZ_HELPER:-$PWD/dist/cleanroom-darwin-vz.app}"`) {
		t.Fatalf("expected ci-darwin-vz-e2e.sh to default to the prebuilt helper app bundle")
	}
	if !strings.Contains(string(content), `export XDG_CACHE_HOME="$tmpdir/cache"`) {
		t.Fatalf("expected ci-darwin-vz-e2e.sh to isolate cleanroom cache under the job tmpdir")
	}
	if !strings.Contains(string(content), `smoke_policy_dir="$tmpdir/smoke-policy"`) {
		t.Fatalf("expected ci-darwin-vz-e2e.sh to create an isolated smoke policy directory")
	}
	if !strings.Contains(string(content), `./dist/cleanroom exec --host "$listen_endpoint" --backend darwin-vz -c "$smoke_policy_dir" -- sh -lc 'echo darwin-vz-e2e'`) {
		t.Fatalf("expected ci-darwin-vz-e2e.sh to use the isolated smoke policy for the smoke test")
	}
	if strings.Contains(string(content), `./dist/cleanroom exec --host "$listen_endpoint" --backend darwin-vz -c "$PWD" -- sh -lc 'echo darwin-vz-e2e'`) {
		t.Fatalf("expected ci-darwin-vz-e2e.sh not to reuse the repository policy for the smoke test")
	}
}

func TestCiDarwinVZFileHandleE2EUsesAllowlistPolicy(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-darwin-vz-filehandle-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-darwin-vz-filehandle-e2e.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		`helper_path="${CLEANROOM_DARWIN_VZ_HELPER:-$PWD/dist/cleanroom-darwin-vz.app}"`,
		`allowlist_policy_dir="$tmpdir/allowlist-policy"`,
		`mode: filehandle`,
		`subnet: 10.233.0.0/24`,
		`gateway_port="$(sed -n 's/.*gateway server started.* addr=[^:]*:\([0-9][0-9]*\).*/\1/p' "$tmpdir/server.log" | tail -n 1)"`,
		`./dist/cleanroom exec --host "$listen_endpoint" --backend darwin-vz -c "$smoke_policy_dir" -- sh -lc "wget -T 20 -S -O - http://10.233.0.1:${gateway_port}/meta/health >/dev/null 2>/tmp/meta.err || true; grep -q 'HTTP/1.1 501 Not Implemented' /tmp/meta.err"`,
		`- host: github.com`,
		`ports: [443]`,
		`./dist/cleanroom exec --host "$listen_endpoint" --backend darwin-vz -c "$allowlist_policy_dir" -- sh -lc 'wget -T 20 -q -O /dev/null https://github.com'`,
		`./dist/cleanroom exec --host "$listen_endpoint" --backend darwin-vz -c "$allowlist_policy_dir" -- sh -lc 'wget -T 20 -q -O /dev/null https://buildkite.com'`,
		`expected non-allowlisted egress to fail in filehandle mode`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-darwin-vz-filehandle-e2e.sh to contain %q", needle)
		}
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

func TestGitHubReleaseWorkflowIsRemoved(t *testing.T) {
	t.Helper()

	_, err := os.Stat("../.github/workflows/release.yml")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected release workflow to be removed, got err=%v", err)
	}
}

func TestBuildkiteReleaseScriptPublishesGitHubRelease(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-buildkite-release.sh")
	if err != nil {
		t.Fatalf("read ci-buildkite-release.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		`fetch_secret CLEANROOM_GITHUB_RELEASE_TOKEN`,
		`[[ -n "${BUILDKITE_TAG:-}" ]] || die "BUILDKITE_TAG is required for release publishing"`,
		`buildkite-agent artifact download "release-extra/darwin_*.tar.gz"`,
		`tar -xzf "${archive_path}" -C "${RELEASE_EXTRA_DIR}"`,
		`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`,
		`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`,
		`goreleaser release --clean`,
		`"${GITHUB_API_BASE}/releases/tags/${BUILDKITE_TAG}"`,
		`"${GITHUB_API_BASE}/releases/assets/${asset_id}"`,
		`"${upload_url}?name=${asset_name}"`,
		`export GITHUB_TOKEN="${github_token}"`,
		`python3 -c 'import json, sys; print(json.load(sys.stdin)["upload_url"].split("{", 1)[0])'`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-buildkite-release.sh to contain %q", needle)
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
		`tar -C release-extra -czf release-extra/darwin_arm64.tar.gz darwin_arm64`,
		`tar -C release-extra -czf release-extra/darwin_amd64.tar.gz darwin_amd64`,
		`buildkite-agent artifact upload "release-extra/darwin_*.tar.gz"`,
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
		`SCRIPT_SOURCE="${BASH_SOURCE[0]-}"`,
		`if [ -n "${SCRIPT_SOURCE}" ] && [ -f "${SCRIPT_SOURCE}" ]; then`,
		`restore_flattened_darwin_helper_bundle()`,
		`local bundle_dir="${extract_dir}/cleanroom-darwin-vz.app"`,
		`mv "${executable_path}" "${bundle_contents_dir}/MacOS/cleanroom-darwin-vz"`,
		`mv "${info_plist_path}" "${bundle_contents_dir}/Info.plist"`,
		`restore_flattened_darwin_helper_bundle "$CLEANROOM_EXTRACT_DIR"`,
		`[ -n "${SCRIPT_DIR}" ] || return 1`,
		`package_script="${SCRIPT_DIR}/package-darwin-vz-helper.sh"`,
		`CLEANROOM_DARWIN_VZ_HELPER_SIGN_KEYCHAIN Optional keychain path when using the repo helper packager`,
		`if ! package_darwin_helper_with_repo_script "${HELPER_BUNDLE_DIR}" "${HELPER_BUNDLE_DIR}"; then`,
		`HELPER_BUNDLE_PROFILE_DEST="${HELPER_BUNDLE_DIR}/Contents/embedded.provisionprofile"`,
		`run_with_optional_sudo rm -f "${HELPER_BUNDLE_PROFILE_DEST}"`,
		`if ! package_darwin_helper_with_repo_script "${HELPER_BINARY_SRC}" "${HELPER_SIGN_TARGET}"; then`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected install.sh to contain %q", needle)
		}
	}
}

func TestInstallScriptPrefersNotarizedMacOSPkgWhenCompatible(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		`can_use_notarized_macos_pkg()`,
		`[ "$HOST_OS" = "Darwin" ] || return 1`,
		`[ "$INSTALL_DIR" = "/usr/local/bin" ] || return 1`,
		`[ "$INSTALL_DARWIN_HELPER" != "0" ] || return 1`,
		`CLEANROOM_DARWIN_VZ_HELPER_PROVISION_PROFILE`,
		`try_install_notarized_macos_pkg()`,
		`pkg_asset="cleanroom_${HOST_OS}_${HOST_ARCH}.pkg"`,
		`download_if_exists "${RELEASE_BASE}/${pkg_asset}" "${pkg_path}"`,
		`download_if_exists "${RELEASE_BASE}/${pkg_asset}.sha256" "${pkg_checksum_path}"`,
		`verify_asset_against_checksum_file "${pkg_asset}" "${pkg_path}" "${pkg_checksum_path}"`,
		`if ! install_macos_pkg "${pkg_path}"; then`,
		`warn "failed to install notarized macOS package: ${pkg_asset}; falling back to archive install"`,
		`return 1`,
		`warn "no notarized macOS pkg found for ${RELEASE_LABEL}; falling back to archive install"`,
		`if can_use_notarized_macos_pkg && try_install_notarized_macos_pkg; then`,
		`log "Installed cleanroom via notarized macOS package"`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected install.sh to contain %q", needle)
		}
	}
}
