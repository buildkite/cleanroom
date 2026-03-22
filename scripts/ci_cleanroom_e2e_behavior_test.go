package scripts_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestCiCleanroomE2ERunPrivilegedUsesConfiguredHelper(t *testing.T) {
	t.Helper()

	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}

	argsPath := filepath.Join(workDir, "helper-args.txt")
	helperPath := filepath.Join(workDir, "helper.sh")
	writeExecutable(t, helperPath, "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >\"$HELPER_ARGS_PATH\"\n")
	writeExecutable(t, filepath.Join(binDir, "sudo"), "#!/usr/bin/env bash\n[[ \"$1\" == \"-n\" ]] && shift\nexec \"$@\"\n")

	scriptPath := scriptAbsPath(t, "ci-cleanroom-e2e.sh")
	_, stderr, err := runShellSnippet(t, `
source "$SCRIPT_PATH"
PRIVILEGED_HELPER_PATH="$HELPER_PATH"
run_privileged capabilities
`, map[string]string{
		"PATH":             binDir + ":" + os.Getenv("PATH"),
		"SCRIPT_PATH":      scriptPath,
		"HELPER_PATH":      helperPath,
		"HELPER_ARGS_PATH": argsPath,
	})
	if err != nil {
		t.Fatalf("runShellSnippet returned error: %v (stderr=%q)", err, stderr)
	}

	got, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if got, want := strings.TrimSpace(string(got)), "capabilities"; got != want {
		t.Fatalf("unexpected helper args: got %q want %q", got, want)
	}
}

func TestCiCleanroomE2EVerifyHelperCapabilitiesDetectsMissingCapabilities(t *testing.T) {
	t.Helper()

	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}

	annotationPath := filepath.Join(workDir, "annotation.md")
	helperPath := filepath.Join(workDir, "helper.sh")
	writeExecutable(t, helperPath, "#!/usr/bin/env bash\nprintf 'unrelated-capability\\n'\n")
	writeExecutable(t, filepath.Join(binDir, "sudo"), "#!/usr/bin/env bash\n[[ \"$1\" == \"-n\" ]] && shift\nexec \"$@\"\n")
	writeExecutable(t, filepath.Join(binDir, "buildkite-agent"), "#!/usr/bin/env bash\ncat >\"$ANNOTATION_FILE\"\n")

	scriptPath := scriptAbsPath(t, "ci-cleanroom-e2e.sh")
	_, stderr, err := runShellSnippet(t, `
source "$SCRIPT_PATH"
PRIVILEGED_HELPER_PATH="$HELPER_PATH"
if verify_helper_capabilities; then
  echo "expected capability verification to fail" >&2
  exit 1
fi
`, map[string]string{
		"PATH":            binDir + ":" + os.Getenv("PATH"),
		"SCRIPT_PATH":     scriptPath,
		"HELPER_PATH":     helperPath,
		"ANNOTATION_FILE": annotationPath,
	})
	if err != nil {
		t.Fatalf("runShellSnippet returned error: %v (stderr=%q)", err, stderr)
	}
	if !strings.Contains(stderr, "missing required capabilities") {
		t.Fatalf("expected missing capabilities in stderr, got %q", stderr)
	}
	if !strings.Contains(stderr, "firecracker-network") {
		t.Fatalf("expected missing capability name in stderr, got %q", stderr)
	}

	annotation, err := os.ReadFile(annotationPath)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if !strings.Contains(string(annotation), "Root helper is missing required capabilities") {
		t.Fatalf("expected annotation to mention missing capabilities, got %q", string(annotation))
	}
}

func TestCiCleanroomE2EVerifyHelperCapabilitiesReportsProbeFailures(t *testing.T) {
	t.Helper()

	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}

	annotationPath := filepath.Join(workDir, "annotation.md")
	helperPath := filepath.Join(workDir, "helper.sh")
	writeExecutable(t, helperPath, "#!/usr/bin/env bash\necho 'probe failed' >&2\nexit 23\n")
	writeExecutable(t, filepath.Join(binDir, "sudo"), "#!/usr/bin/env bash\n[[ \"$1\" == \"-n\" ]] && shift\nexec \"$@\"\n")
	writeExecutable(t, filepath.Join(binDir, "buildkite-agent"), "#!/usr/bin/env bash\ncat >\"$ANNOTATION_FILE\"\n")

	scriptPath := scriptAbsPath(t, "ci-cleanroom-e2e.sh")
	_, stderr, err := runShellSnippet(t, `
source "$SCRIPT_PATH"
PRIVILEGED_HELPER_PATH="$HELPER_PATH"
if verify_helper_capabilities; then
  echo "expected capability verification to fail" >&2
  exit 1
fi
`, map[string]string{
		"PATH":            binDir + ":" + os.Getenv("PATH"),
		"SCRIPT_PATH":     scriptPath,
		"HELPER_PATH":     helperPath,
		"ANNOTATION_FILE": annotationPath,
	})
	if err != nil {
		t.Fatalf("runShellSnippet returned error: %v (stderr=%q)", err, stderr)
	}
	if !strings.Contains(stderr, "Root helper capability probe failed") {
		t.Fatalf("expected probe failure in stderr, got %q", stderr)
	}

	annotation, err := os.ReadFile(annotationPath)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if !strings.Contains(string(annotation), "Root helper capability probe failed") {
		t.Fatalf("expected annotation to mention probe failure, got %q", string(annotation))
	}
}

func runShellSnippet(t *testing.T, snippet string, env map[string]string) (string, string, error) {
	t.Helper()

	cmd := exec.Command("bash", "-c", snippet)
	cmd.Env = envSlice(env)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func scriptAbsPath(t *testing.T, name string) string {
	t.Helper()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd returned error: %v", err)
	}
	return filepath.Join(cwd, name)
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) returned error: %v", path, err)
	}
}

func envSlice(overrides map[string]string) []string {
	envMap := make(map[string]string)
	for _, entry := range os.Environ() {
		parts := strings.SplitN(entry, "=", 2)
		value := ""
		if len(parts) == 2 {
			value = parts[1]
		}
		envMap[parts[0]] = value
	}
	for key, value := range overrides {
		envMap[key] = value
	}

	keys := make([]string, 0, len(envMap))
	for key := range envMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+envMap[key])
	}
	return env
}
