package scripts_test

import (
	"errors"
	"os"
	"strings"
	"testing"
)

const miseBuildkitePluginRef = "github.com/lox/mise-buildkite-plugin#a172963b3d34e98601e2a65c7dd08211fb49b7f0"

func TestBuildkitePipelineUsesMisePlugin(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("../.buildkite/pipeline.yml")
	if err != nil {
		t.Fatalf("read .buildkite/pipeline.yml: %v", err)
	}

	pipeline := string(content)
	if !strings.Contains(pipeline, miseBuildkitePluginRef) {
		t.Fatalf("expected .buildkite/pipeline.yml to use %q", miseBuildkitePluginRef)
	}
	if strings.Contains(pipeline, "add_shims_to_path: false") {
		t.Fatalf("expected .buildkite/pipeline.yml to keep mise shims enabled in CI")
	}
	if strings.Contains(pipeline, "command: mise run") {
		t.Fatalf("expected .buildkite/pipeline.yml to avoid direct `mise run` step commands")
	}
	if !strings.Contains(pipeline, "command: scripts/ci-darwin-vz-filehandle-e2e.sh") {
		t.Fatalf("expected .buildkite/pipeline.yml to include the darwin-vz filehandle e2e step")
	}
	if !strings.Contains(pipeline, "command: scripts/ci-macos-release-pkg.sh") {
		t.Fatalf("expected .buildkite/pipeline.yml to include the macOS release pkg step")
	}
	if !strings.Contains(pipeline, "command: scripts/ci-buildkite-release.sh") {
		t.Fatalf("expected .buildkite/pipeline.yml to include the Buildkite release publish step")
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
}

func TestBuildkiteCommandHookIsRemoved(t *testing.T) {
	t.Parallel()

	_, err := os.Stat("../.buildkite/hooks/command")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected .buildkite/hooks/command to be removed, got err=%v", err)
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

	mise := string(content)
	for _, needle := range []string{
		"[tasks.\"ci:bootstrap:linux\"]",
		"run = \"scripts/ci-bootstrap-linux-ssm.sh run\"",
		"[tasks.\"ci:bootstrap:linux:logs\"]",
		"run = \"scripts/ci-bootstrap-linux-ssm.sh logs\"",
	} {
		if !strings.Contains(mise, needle) {
			t.Fatalf("expected mise.toml to contain %q", needle)
		}
	}
}
