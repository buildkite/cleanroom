package scripts_test

import (
	"os"
	"testing"
)

func TestCiDarwinVZVMNetNormalizeSecretValueRemovesCarriageReturns(t *testing.T) {
	t.Helper()

	scriptPath := scriptAbsPath(t, "ci-darwin-vz-vmnet-e2e.sh")
	stdout, stderr, err := runShellSnippet(t, `
source "$SCRIPT_PATH"
normalize_secret_value $'line1\r\nline2\r'
`, map[string]string{
		"SCRIPT_PATH": scriptPath,
	})
	if err != nil {
		t.Fatalf("runShellSnippet returned error: %v (stderr=%q)", err, stderr)
	}
	if got, want := stdout, "line1\nline2"; got != want {
		t.Fatalf("unexpected normalized secret: got %q want %q", got, want)
	}
}

func TestCiDarwinVZVMNetResolveLocalHelperPathPrefersEnv(t *testing.T) {
	t.Helper()

	scriptPath := scriptAbsPath(t, "ci-darwin-vz-vmnet-e2e.sh")
	stdout, stderr, err := runShellSnippet(t, `
source "$SCRIPT_PATH"
resolve_local_helper_path
`, map[string]string{
		"SCRIPT_PATH":                scriptPath,
		"CLEANROOM_DARWIN_VZ_HELPER": "/tmp/custom-helper.app",
	})
	if err != nil {
		t.Fatalf("runShellSnippet returned error: %v (stderr=%q)", err, stderr)
	}
	if got, want := stdout, "/tmp/custom-helper.app\n"; got != want {
		t.Fatalf("unexpected helper path: got %q want %q", got, want)
	}
}

func TestEnvSliceAppliesOverridesLast(t *testing.T) {
	t.Helper()

	env := envSlice(map[string]string{"PATH": "/tmp/bin", "TEST_KEY": "value"})
	var (
		foundPATH bool
		foundKey  bool
	)
	for _, entry := range env {
		if entry == "PATH=/tmp/bin" {
			foundPATH = true
		}
		if entry == "TEST_KEY=value" {
			foundKey = true
		}
	}
	if !foundPATH || !foundKey {
		t.Fatalf("expected env overrides to be present, got %v", env)
	}
	if _, ok := os.LookupEnv("PATH"); !ok {
		t.Fatal("expected PATH to exist in process environment")
	}
}
