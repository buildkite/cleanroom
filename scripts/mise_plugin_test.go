package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVendoredMisePluginPreCommandSucceedsWhenSourced(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	workDir := t.TempDir()
	fakeBinDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(fakeBinDir, 0o755); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}

	payloadDir := filepath.Join(workDir, "payload", "mise", "bin")
	if err := os.MkdirAll(payloadDir, 0o755); err != nil {
		t.Fatalf("mkdir payload: %v", err)
	}

	fakeMisePath := filepath.Join(payloadDir, "mise")
	if err := os.WriteFile(fakeMisePath, []byte(`#!/bin/bash
set -euo pipefail

cmd="${1:-}"
case "$cmd" in
  --version)
    echo "mise 2026.3.9"
    ;;
  install)
    ;;
  env)
    if [[ "${2:-}" == "--dotenv" ]]; then
      printf 'FAKE_TOOL=1\n'
    fi
    ;;
esac
`), 0o755); err != nil {
		t.Fatalf("write fake mise: %v", err)
	}

	tarballPath := filepath.Join(workDir, "mise.tar.gz")
	tarCmd := exec.Command("tar", "-czf", tarballPath, "-C", filepath.Join(workDir, "payload"), ".")
	if out, err := tarCmd.CombinedOutput(); err != nil {
		t.Fatalf("create fake mise tarball: %v\n%s", err, out)
	}

	fakeCurlPath := filepath.Join(fakeBinDir, "curl")
	if err := os.WriteFile(fakeCurlPath, []byte(`#!/bin/bash
set -euo pipefail

url=""
for arg in "$@"; do
  url="$arg"
done

case "$url" in
  *"/VERSION")
    printf '2026.3.9'
    ;;
  *)
    cat "$FAKE_MISE_TARBALL"
    ;;
esac
`), 0o755); err != nil {
		t.Fatalf("write fake curl: %v", err)
	}

	envFile := filepath.Join(workDir, "buildkite.env")
	if err := os.WriteFile(envFile, nil, 0o644); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	cmd := exec.Command("bash", "-c", ". ./.buildkite/plugins/mise/hooks/pre-command")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"BUILDKITE_ENV_FILE="+envFile,
		"BUILDKITE_BUILD_CHECKOUT_PATH="+repoRoot,
		"FAKE_MISE_TARBALL="+tarballPath,
		"MISE_DATA_DIR="+filepath.Join(workDir, "mise-data"),
		"PATH="+fakeBinDir+":"+os.Getenv("PATH"),
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("source vendored mise hook: %v\n%s", err, out)
	}

	envContent, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if !strings.Contains(string(envContent), "export MISE_DATA_DIR=") {
		t.Fatalf("expected env file to export MISE_DATA_DIR, got:\n%s", envContent)
	}
}
