package scripts_test

import (
	"errors"
	"os"
	"strings"
	"testing"
)

const miseBuildkitePluginRef = "github.com/lox/mise-buildkite-plugin#a5845c5082d3a4fe36dd77ae74973dfc86fc91a2"
const miseBuildkitePluginVersion = "2026.5.12"
const setupGoBuildkitePluginRef = "github.com/buildkite-plugins/setup-go-buildkite-plugin#daa7af945245588f85b76ba7fe0a9af3d87dbf91"

func TestBuildkitePipelineUsesSetupGoForGoSteps(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("../.buildkite/pipeline.yml")
	if err != nil {
		t.Fatalf("read .buildkite/pipeline.yml: %v", err)
	}

	pipeline := string(content)
	if strings.Contains(pipeline, "command: mise run") {
		t.Fatalf("expected .buildkite/pipeline.yml to avoid direct `mise run` step commands")
	}

	for _, snippet := range []string{
		`- label: ":shell: Shellcheck"
    plugins:
      - ` + miseBuildkitePluginRef + `:
          version: "` + miseBuildkitePluginVersion + `"
    command: shellcheck`,
		`- label: ":test_tube: Test (Linux)"
    plugins:
      - ` + setupGoBuildkitePluginRef + `:
    command: go test ./...`,
		`- label: ":test_tube: Test (macOS)"
    plugins:
      - ` + setupGoBuildkitePluginRef + `:
    command: go test ./...`,
		`- label: ":apple: E2E (darwin-vz)"
    plugins:
      - ` + setupGoBuildkitePluginRef + `:
    command: scripts/ci-with-host-lock.sh cleanroom-darwin-vz scripts/ci-darwin-vz-e2e.sh`,
		`- label: ":apple: E2E (darwin-vz filehandle)"
    plugins:
      - ` + setupGoBuildkitePluginRef + `:
    command: scripts/ci-with-host-lock.sh cleanroom-darwin-vz scripts/ci-darwin-vz-filehandle-e2e.sh`,
		`- label: ":package: macOS release pkg"
    key: macos-release-pkg
    if: build.tag != null || build.branch == "codex/macos-notarized-release-pkg"
    plugins:
      - ` + setupGoBuildkitePluginRef + `:
    command: scripts/ci-macos-release-pkg.sh`,
		`- label: ":penguin: darwin-vz kernel release assets"
    key: darwin-vz-kernel-release-assets
    if: build.tag != null
    command: scripts/ci-darwin-vz-kernel-release.sh`,
		`- label: ":fire: E2E (Firecracker)"
    plugins:
      - ` + setupGoBuildkitePluginRef + `:
    command: scripts/ci-with-host-lock.sh cleanroom-firecracker scripts/ci-cleanroom-e2e.sh`,
		`- label: ":book: Examples (macOS)"
    plugins:
      - ` + setupGoBuildkitePluginRef + `:
    command: scripts/ci-with-host-lock.sh cleanroom-darwin-vz scripts/ci-examples-darwin-vz.sh`,
		`- label: ":book: Examples (Linux)"
    plugins:
      - ` + setupGoBuildkitePluginRef + `:
    command: scripts/ci-with-host-lock.sh cleanroom-firecracker scripts/ci-examples-firecracker.sh`,
		`- label: ":rocket: Publish release"
    if: build.tag != null
    plugins:
      - ` + miseBuildkitePluginRef + `:
          version: "` + miseBuildkitePluginVersion + `"
    command: scripts/ci-buildkite-release.sh`,
	} {
		if !strings.Contains(pipeline, snippet) {
			t.Fatalf("expected .buildkite/pipeline.yml to contain step snippet:\n%s", snippet)
		}
	}

	if !strings.Contains(pipeline, "scripts/ci-darwin-vz-filehandle-e2e.sh") {
		t.Fatalf("expected .buildkite/pipeline.yml to include the darwin-vz filehandle e2e step")
	}
	if !strings.Contains(pipeline, "command: scripts/ci-macos-release-pkg.sh") {
		t.Fatalf("expected .buildkite/pipeline.yml to include the macOS release pkg step")
	}
	if !strings.Contains(pipeline, "command: scripts/ci-darwin-vz-kernel-release.sh") {
		t.Fatalf("expected .buildkite/pipeline.yml to include the darwin-vz kernel release step")
	}
	if !strings.Contains(pipeline, "command: scripts/ci-buildkite-release.sh") {
		t.Fatalf("expected .buildkite/pipeline.yml to include the Buildkite release publish step")
	}
	for _, needle := range []string{
		"scripts/base-image-tag.sh",
		"scripts/install-global.sh",
		"scripts/e2e-observability.sh",
		"scripts/ci-with-host-lock.sh",
		"scripts/ci-example-smoke.sh",
		"scripts/ci-examples-firecracker.sh",
		"scripts/ci-examples-darwin-vz.sh",
		"scripts/build-macos-release-pkg.sh",
		"scripts/notarize-macos-package.sh",
		"scripts/build-darwin-vz-minimal-kernel-release.sh",
		"scripts/ci-darwin-vz-kernel-release.sh",
	} {
		if !strings.Contains(pipeline, needle) {
			t.Fatalf("expected .buildkite/pipeline.yml shellcheck command to include %q", needle)
		}
	}
	for _, needle := range []string{
		"scripts/bootstrap-buildkite-agent.sh",
		"scripts/bootstrap-buildkite-agent-macos.sh",
		"scripts/ci-bootstrap-linux-ssm.sh",
	} {
		if strings.Contains(pipeline, needle) {
			t.Fatalf("expected .buildkite/pipeline.yml shellcheck command not to include %q", needle)
		}
	}
	if !strings.Contains(pipeline, "queue: cleanroom-mac-signer") {
		t.Fatalf("expected .buildkite/pipeline.yml to route the macOS release pkg step to cleanroom-mac-signer")
	}
	if !strings.Contains(pipeline, `CLEANROOM_SIGNING_JOB: "1"`) {
		t.Fatalf("expected .buildkite/pipeline.yml to mark the macOS release pkg step as a signing job")
	}
	if !strings.Contains(pipeline, `- wait`) {
		t.Fatalf("expected .buildkite/pipeline.yml to gate Buildkite release publishing behind a wait step")
	}
	if !strings.Contains(pipeline, "CLEANROOM_DARWIN_VZ_HELPER_SIGN_IDENTIFIER: com.buildkite.cleanroom.darwin-vz") {
		t.Fatalf("expected .buildkite/pipeline.yml to set the darwin-vz vmnet helper bundle identifier")
	}
	for _, needle := range []string{
		"concurrency_group: cleanroom-e2e",
		"concurrency_group: cleanroom-darwin-vz-e2e",
	} {
		if strings.Contains(pipeline, needle) {
			t.Fatalf("expected .buildkite/pipeline.yml to use host locks instead of global concurrency group %q", needle)
		}
	}
	for _, needle := range []string{
		"CLEANROOM_KERNEL_IMAGE",
		"CLEANROOM_FIRECRACKER_BINARY",
		"CLEANROOM_PRIVILEGED_MODE",
		"CLEANROOM_PRIVILEGED_HELPER_PATH",
	} {
		if strings.Contains(pipeline, needle) {
			t.Fatalf("expected .buildkite/pipeline.yml not to hardcode Firecracker CI env %q", needle)
		}
	}
}

func TestBuildkiteCommandHookIsRemoved(t *testing.T) {
	t.Parallel()

	_, err := os.Stat("../.buildkite/hooks/command")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected .buildkite/hooks/command to be removed, got err=%v", err)
	}
}

func TestBuildkiteHostLockWrapperUsesMachineScopedAgentLocks(t *testing.T) {
	t.Parallel()

	info, err := os.Stat("ci-with-host-lock.sh")
	if err != nil {
		t.Fatalf("stat ci-with-host-lock.sh: %v", err)
	}
	if info.Mode()&0111 == 0 {
		t.Fatalf("expected ci-with-host-lock.sh to be executable")
	}

	content, err := os.ReadFile("ci-with-host-lock.sh")
	if err != nil {
		t.Fatalf("read ci-with-host-lock.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		`workspace_isolation_enabled()`,
		`CLEANROOM_CI_ISOLATE_WORKSPACE:-auto`,
		`CLEANROOM_CI_WORKSPACE_PARENT:-${TMPDIR:-/tmp}`,
		`source_origin_url="$(git config --get remote.origin.url || true)"`,
		`git clone --local --no-hardlinks --quiet "$PWD" "$workspace_dir"`,
		`git -C "$workspace_dir" remote set-url origin "$source_origin_url"`,
		`cd "$workspace_dir" && "$@"`,
		`trap release_buildkite_lock EXIT`,
		`buildkite-agent lock acquire --lock-wait-timeout "$buildkite_lock_wait_timeout" "$lock_key"`,
		`buildkite-agent lock release "$lock_key" "$token"`,
		`buildkite-agent lock release failed for: $lock_key`,
		`buildkite-agent lock acquire failed; falling back to host file lock`,
		`grep -Eiq 'timeout|timed out|deadline' "$acquire_err"`,
		`CLEANROOM_BUILDKITE_LOCK_WAIT_TIMEOUT:-${BUILDKITE_LOCK_WAIT_TIMEOUT:-45m}`,
		`CLEANROOM_CI_HOST_LOCK_DIR:-/tmp/cleanroom-ci-host-locks`,
		`chmod 1777 "$lock_dir"`,
		`chmod 666 "$lock_file"`,
		`flock "$lock_fd"`,
		`lockf "$lock_file" "$SCRIPT_PATH" --internal-run-wrapped "$@"`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-with-host-lock.sh to contain %q", needle)
		}
	}
	if strings.Contains(script, `buildkite-agent lock release "$lock_key" "$token" || true`) {
		t.Fatalf("expected ci-with-host-lock.sh to fail successful jobs when Buildkite lock release fails")
	}
}

func TestBuildkiteVendoredMisePluginIsRemoved(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"../.buildkite/plugins/mise/plugin.yml",
		"../.buildkite/plugins/mise/hooks/pre-command",
	} {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			_, err := os.Stat(path)
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("expected %s to be removed, got err=%v", path, err)
			}
		})
	}
}

func TestDeprecatedRootFSHelperScriptsAreRemoved(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"create-rootfs-image.sh",
		"prepare-firecracker-image.sh",
	} {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			_, err := os.Stat(path)
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("expected %s to be removed, got err=%v", path, err)
			}
		})
	}
}

func TestBuildkiteCIScriptsDoNotInvokeMiseDirectly(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"ci-cleanroom-e2e.sh",
		"ci-with-host-lock.sh",
		"ci-example-smoke.sh",
		"ci-examples-firecracker.sh",
		"ci-examples-darwin-vz.sh",
		"ci-darwin-vz-e2e.sh",
		"ci-darwin-vz-filehandle-e2e.sh",
		"ci-macos-release-pkg.sh",
		"ci-darwin-vz-kernel-release.sh",
		"ci-buildkite-release.sh",
	} {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}

			script := string(content)
			if strings.Contains(script, "mise run ") || strings.Contains(script, "mise exec ") {
				t.Fatalf("expected %s to use the Buildkite plugin environment instead of invoking mise directly", path)
			}
			if path == "ci-cleanroom-e2e.sh" && strings.Contains(script, "go run ./scripts/download_sandbox_file") {
				t.Fatalf("expected %s to use the prebuilt download helper instead of `go run`", path)
			}
		})
	}
}

func TestMiseIncludesLinuxBootstrapTasks(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("../mise.toml")
	if err != nil {
		t.Fatalf("read mise.toml: %v", err)
	}

	if strings.Contains(string(content), "ci-bootstrap-linux-ssm.sh") {
		t.Fatalf("expected public mise.toml not to expose private bootstrap rerun tasks")
	}
	if strings.Contains(string(content), "bootstrap-buildkite-agent.sh") {
		t.Fatalf("expected public mise.toml not to lint private bootstrap scripts")
	}
}

func TestMiseLintShellCoversSharedE2EObservabilityHelper(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("../mise.toml")
	if err != nil {
		t.Fatalf("read mise.toml: %v", err)
	}

	mise := string(content)
	for _, needle := range []string{
		`[tasks.lint-shell]`,
		`scripts/base-image-tag.sh`,
		`scripts/e2e-observability.sh`,
		`scripts/ci-with-host-lock.sh`,
		`scripts/ci-example-smoke.sh`,
		`scripts/ci-examples-firecracker.sh`,
		`scripts/ci-examples-darwin-vz.sh`,
		`scripts/ci-darwin-vz-filehandle-e2e.sh`,
		`scripts/build-macos-release-pkg.sh`,
		`scripts/notarize-macos-package.sh`,
		`scripts/ci-macos-release-pkg.sh`,
		`scripts/build-darwin-vz-minimal-kernel-release.sh`,
		`scripts/ci-darwin-vz-kernel-release.sh`,
		`scripts/ci-buildkite-release.sh`,
	} {
		if !strings.Contains(mise, needle) {
			t.Fatalf("expected mise.toml to contain %q", needle)
		}
	}
}
