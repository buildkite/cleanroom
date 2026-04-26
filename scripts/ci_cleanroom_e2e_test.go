package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestCiCleanroomE2EUsesHelperViaNonInteractiveSudo(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-cleanroom-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-cleanroom-e2e.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		"PRIVILEGED_HELPER_PATH=\"${CLEANROOM_PRIVILEGED_HELPER_PATH:-/usr/local/sbin/cleanroom-root-helper}\"",
		"sudo -n \"$PRIVILEGED_HELPER_PATH\" \"$@\"",
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-cleanroom-e2e.sh to contain %q", needle)
		}
	}

	for _, needle := range []string{
		"CLEANROOM_PRIVILEGED_MODE",
		"sudo -n \"$@\"",
	} {
		if strings.Contains(script, needle) {
			t.Fatalf("expected ci-cleanroom-e2e.sh not to contain %q", needle)
		}
	}
}

func TestFirecrackerCIScriptsDoNotRequireKernelEnv(t *testing.T) {
	t.Helper()

	for _, path := range []string{
		"ci-cleanroom-e2e.sh",
		"ci-examples-firecracker.sh",
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		script := string(content)
		if strings.Contains(script, "CLEANROOM_KERNEL_IMAGE is required") {
			t.Fatalf("expected %s not to require CLEANROOM_KERNEL_IMAGE", path)
		}
		if !strings.Contains(script, "if [[ -n \"$KERNEL_IMAGE\" ]]; then") {
			t.Fatalf("expected %s to append kernel_image only when explicitly configured", path)
		}
	}
}

func TestCiCleanroomE2EDownloadSandboxFileProbeUsesTimeout(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-cleanroom-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-cleanroom-e2e.sh: %v", err)
	}

	if !strings.Contains(string(content), "--timeout 45s") {
		t.Fatalf("expected ci-cleanroom-e2e.sh to bound download_sandbox_file calls with --timeout")
	}
}

func TestCiCleanroomE2EProbesHelperCapabilitiesInsteadOfHelperDrift(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-cleanroom-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-cleanroom-e2e.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		"ROOT_HELPER_REQUIRED_CAPABILITIES=(",
		"sudo -n \"$PRIVILEGED_HELPER_PATH\" capabilities",
		"verify_helper_capabilities",
		"Roll out the latest helper on the CI host",
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-cleanroom-e2e.sh to contain %q", needle)
		}
	}

	for _, needle := range []string{
		"ROOT_HELPER_LEGACY_CAPABILITIES=(",
		"unsupported command 'capabilities'",
		"assuming baseline helper capabilities",
		"sha256sum scripts/cleanroom-root-helper.sh",
		"Update with: sudo install -o root -g root -m 0755 scripts/cleanroom-root-helper.sh",
	} {
		if strings.Contains(script, needle) {
			t.Fatalf("expected ci-cleanroom-e2e.sh not to contain %q", needle)
		}
	}
}

func TestCiCleanroomE2EReusedSandboxExecOmitsChdir(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-cleanroom-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-cleanroom-e2e.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		"./dist/cleanroom exec --host \"$listen_endpoint\" --in \"$sandbox_id\" -- sh -lc 'printf persisted-data >/tmp/persist.txt'",
		"./dist/cleanroom exec --host \"$listen_endpoint\" --in \"$sandbox_id\" -- sh -lc 'cat /tmp/persist.txt' | tee \"$tmpdir/persist-read.out\"",
		"./dist/cleanroom exec --host \"$listen_endpoint\" --in \"$sandbox_id\" -- sh -lc 'echo should-not-run' >\"$tmpdir/terminated.out\" 2>\"$tmpdir/terminated.err\"",
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-cleanroom-e2e.sh to contain %q", needle)
		}
	}

	if strings.Contains(script, "./dist/cleanroom exec --host \"$listen_endpoint\" -c \"$PWD\" --in \"$sandbox_id\"") {
		t.Fatal("expected ci-cleanroom-e2e.sh not to pass --chdir when reusing a sandbox")
	}
}

func TestCiCleanroomE2EIsolatesCacheInTempDir(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-cleanroom-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-cleanroom-e2e.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		`export XDG_CACHE_HOME="$tmpdir/cache"`,
		`mkdir -p "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME" "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR" "$XDG_DATA_HOME"`,
		`./dist/cleanroom exec --host "$listen_endpoint" -c "$PWD" -- sh -lc 'echo cleanroom-e2e'`,
		`./dist/cleanroom exec --host "$listen_endpoint" -c "$PWD" -- sh -lc '`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-cleanroom-e2e.sh to contain %q", needle)
		}
	}
}

func TestCiCleanroomE2EPublishesLaunchObservabilityBundle(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("ci-cleanroom-e2e.sh")
	if err != nil {
		t.Fatalf("read ci-cleanroom-e2e.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		`source "$SCRIPT_DIR/e2e-observability.sh"`,
		`OBSERVABILITY_ARCHIVE_NAME="firecracker-e2e-observability.tgz"`,
		`capture_latest_execution_observability "./dist/cleanroom"`,
		`require_launch_observability "$OBSERVABILITY_SUITE_LABEL"`,
		`publish_buildkite_observability \`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected ci-cleanroom-e2e.sh to contain %q", needle)
		}
	}

	for _, needle := range []string{
		`OBSERVABILITY_CONTEXT="cleanroom-e2e-observability"`,
		`./dist/cleanroom status --last | tee "$tmpdir/status.out"`,
		`buildkite-agent annotate --context cleanroom-e2e-observability --style info < "$annotation_file"`,
	} {
		if strings.Contains(script, needle) {
			t.Fatalf("expected ci-cleanroom-e2e.sh not to contain %q", needle)
		}
	}
}
