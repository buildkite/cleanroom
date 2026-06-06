package scripts_test

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestBuildkiteCommandHookIsRemoved(t *testing.T) {
	t.Parallel()

	_, err := os.Stat("../.buildkite/hooks/command")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected .buildkite/hooks/command to be removed, got err=%v", err)
	}
}

func TestBuildkitePreCommandHookMintsTestEngineOIDCToken(t *testing.T) {
	t.Parallel()

	info, err := os.Stat("../.buildkite/hooks/pre-command")
	if err != nil {
		t.Fatalf("stat .buildkite/hooks/pre-command: %v", err)
	}
	if info.Mode()&0111 == 0 {
		t.Fatalf("expected .buildkite/hooks/pre-command to be executable")
	}

	content, err := os.ReadFile("../.buildkite/hooks/pre-command")
	if err != nil {
		t.Fatalf("read .buildkite/hooks/pre-command: %v", err)
	}

	hook := string(content)
	for _, needle := range []string{
		`BUILDKITE_TEST_ENGINE_SUITE_SLUG`,
		`BUILDKITE_ANALYTICS_TOKEN`,
		`BUILDKITE_TEST_ENGINE_OIDC`,
		`buildkite-agent oidc request-token --audience "$suite_url" --lifetime "$lifetime_seconds"`,
		`https://buildkite.com/organizations/${organization_slug}/analytics/suites/${BUILDKITE_TEST_ENGINE_SUITE_SLUG}`,
	} {
		if !strings.Contains(hook, needle) {
			t.Fatalf("expected .buildkite/hooks/pre-command to contain %q", needle)
		}
	}
}

func TestBuildkitePublishSchemaUsesMiseTask(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("../.buildkite/pipeline.yml")
	if err != nil {
		t.Fatalf("read .buildkite/pipeline.yml: %v", err)
	}

	pipeline := string(content)
	if !strings.Contains(pipeline, `secrets:
      - BUF_TOKEN`) {
		t.Fatalf("expected publish schema step to request BUF_TOKEN as a secret")
	}
	if !strings.Contains(pipeline, `command: mise run proto:push`) {
		t.Fatalf("expected publish schema step to use the proto:push mise task")
	}
	for _, needle := range []string{
		`${BUF_TOKEN`,
		`$${BUF_TOKEN`,
		`buf push --git-metadata`,
	} {
		if strings.Contains(pipeline, needle) {
			t.Fatalf("expected publish schema step not to inline %q in the pipeline command", needle)
		}
	}
}

func TestBuildkiteAuthOIDCSmokeUsesRealBuildkiteToken(t *testing.T) {
	t.Parallel()

	info, err := os.Stat("ci-auth-oidc-smoke.sh")
	if err != nil {
		t.Fatalf("stat ci-auth-oidc-smoke.sh: %v", err)
	}
	if info.Mode()&0111 == 0 {
		t.Fatalf("expected ci-auth-oidc-smoke.sh to be executable")
	}

	content, err := os.ReadFile("ci-auth-oidc-smoke.sh")
	if err != nil {
		t.Fatalf("read ci-auth-oidc-smoke.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		`buildkite-agent oidc request-token`,
		`--subject-claim pipeline_id`,
		`--claim organization_id,pipeline_id`,
		`https://agent.buildkite.com/.well-known/jwks`,
		`required_claims:`,
		`resource.owner.principal_id == principal.id`,
		`resource.owner.scope == principal.scope`,
		`expect_auth_check "allowed sandbox create" true`,
		`expect_auth_check "denied sandbox create for another repository" false`,
		`expect_auth_check "allowed same-owner sandbox get" true`,
		`expect_auth_check "denied cross-principal sandbox get" false`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-auth-oidc-smoke.sh to contain %q", needle)
		}
	}
	for _, needle := range []string{
		`cat "$token_path"`,
		`echo "$token`,
		`set -x`,
	} {
		if strings.Contains(script, needle) {
			t.Fatalf("expected ci-auth-oidc-smoke.sh not to expose the token through %q", needle)
		}
	}
}

func TestBuildkiteGoTestEngineScriptBootstrapsBktecAndRequiresGotestsum(t *testing.T) {
	t.Parallel()

	info, err := os.Stat("ci-go-test-engine.sh")
	if err != nil {
		t.Fatalf("stat ci-go-test-engine.sh: %v", err)
	}
	if info.Mode()&0111 == 0 {
		t.Fatalf("expected ci-go-test-engine.sh to be executable")
	}

	content, err := os.ReadFile("ci-go-test-engine.sh")
	if err != nil {
		t.Fatalf("read ci-go-test-engine.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		`BKTEC_VERSION="${BKTEC_VERSION:-2.6.0}"`,
		`github.com/buildkite/test-engine-client/releases/download/v${BKTEC_VERSION}/${asset}`,
		`if [[ "${BUILDKITE_PARALLEL_JOB_COUNT:-0}" == "0" ]]; then`,
		`BUILDKITE_PARALLEL_JOB_COUNT=1`,
		`goos="$(go env GOOS)"`,
		`BUILDKITE_TEST_ENGINE_RESULT_PATH="tmp/test-engine/gotest-${goos}-${BUILDKITE_PARALLEL_JOB:-0}.xml"`,
		`BUILDKITE_TEST_ENGINE_SUITE_SLUG is required`,
		`command -v gotestsum`,
		`gotestsum is required; install it with mise before running Test Engine`,
		`bktec run`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-go-test-engine.sh to contain %q", needle)
		}
	}
	if strings.Contains(script, `go install "gotest.tools/gotestsum`) {
		t.Fatalf("expected ci-go-test-engine.sh to rely on mise-installed gotestsum")
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
		`trap release_buildkite_lock EXIT`,
		`buildkite-agent is required for host locks`,
		`buildkite-agent lock acquire --lock-wait-timeout "$buildkite_lock_wait_timeout" "$lock_key"`,
		`buildkite-agent lock release "$lock_key" "$token"`,
		`buildkite-agent lock release failed for: $lock_key`,
		`CLEANROOM_BUILDKITE_LOCK_WAIT_TIMEOUT:-${BUILDKITE_LOCK_WAIT_TIMEOUT:-45m}`,
		`"$@"`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-with-host-lock.sh to contain %q", needle)
		}
	}
	if strings.Contains(script, `buildkite-agent lock release "$lock_key" "$token" || true`) {
		t.Fatalf("expected ci-with-host-lock.sh to fail successful jobs when Buildkite lock release fails")
	}
	for _, needle := range []string{
		`falling back to host file lock`,
		`CLEANROOM_CI_HOST_LOCK_DIR`,
		`flock`,
		`lockf`,
		`CLEANROOM_CI_ISOLATE_WORKSPACE`,
		`CLEANROOM_CI_WORKSPACE_PARENT`,
		`git clone`,
		`cleanroom-ci-workspace`,
	} {
		if strings.Contains(script, needle) {
			t.Fatalf("expected ci-with-host-lock.sh to stay a Buildkite lock wrapper only, found %q", needle)
		}
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
		"ci-go-test-engine.sh",
		"ci-auth-oidc-smoke.sh",
		"ci-example-smoke.sh",
		"ci-examples-firecracker.sh",
		"ci-examples-darwin-vz.sh",
		"ci-darwin-vz-e2e.sh",
		"ci-darwin-vz-filehandle-e2e.sh",
		"ci-macos-release-pkg.sh",
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

	if !strings.Contains(string(content), `gotestsum = "1.13.0"`) {
		t.Fatalf("expected public mise.toml to pin gotestsum for Test Engine")
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
		`.buildkite/hooks/pre-command`,
		`scripts/base-image-tag.sh`,
		`scripts/e2e-observability.sh`,
		`scripts/ci-with-host-lock.sh`,
		`scripts/ci-go-test-engine.sh`,
		`scripts/ci-auth-oidc-smoke.sh`,
		`scripts/ci-example-smoke.sh`,
		`scripts/ci-examples-firecracker.sh`,
		`scripts/ci-examples-darwin-vz.sh`,
		`scripts/ci-darwin-vz-filehandle-e2e.sh`,
		`scripts/build-macos-release-pkg.sh`,
		`scripts/notarize-macos-package.sh`,
		`scripts/ci-macos-release-pkg.sh`,
		`scripts/ci-buildkite-release.sh`,
	} {
		if !strings.Contains(mise, needle) {
			t.Fatalf("expected mise.toml to contain %q", needle)
		}
	}
}
