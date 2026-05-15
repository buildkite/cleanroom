package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

type policyValidateLoader struct {
	compiled  *policy.CompiledPolicy
	source    string
	loadedCWD string
}

func (l *policyValidateLoader) LoadAndCompile(cwd string) (*policy.CompiledPolicy, string, error) {
	l.loadedCWD = cwd
	return l.compiled, l.source, nil
}

func (l *policyValidateLoader) LoadRepository(string) (policy.RepositoryConfig, string, error) {
	return policy.RepositoryConfig{}, "", nil
}

func (l *policyValidateLoader) LoadExpose(string) (policy.ExposeConfig, string, error) {
	return policy.ExposeConfig{}, "", nil
}

func TestPolicyValidateCommandRunJSON(t *testing.T) {
	t.Parallel()

	loader := &policyValidateLoader{
		compiled: &policy.CompiledPolicy{
			Hash:     "policy-hash-json",
			ImageRef: "ghcr.io/buildkite/cleanroom-base/alpine@sha256:6666666666666666666666666666666666666666666666666666666666666666",
		},
		source: "/repo/cleanroom.yaml",
	}

	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err := (&PolicyValidateCommand{JSON: true}).Run(&runtimeContext{
		CWD:    t.TempDir(),
		Stdout: stdout,
		Loader: loader,
	})
	if err != nil {
		t.Fatalf("PolicyValidateCommand.Run returned error: %v", err)
	}

	var payload struct {
		Source string                `json:"source"`
		Policy policy.CompiledPolicy `json:"policy"`
	}
	if err := json.Unmarshal([]byte(readStdout()), &payload); err != nil {
		t.Fatalf("unmarshal JSON output: %v", err)
	}
	if got, want := payload.Source, loader.source; got != want {
		t.Fatalf("unexpected source in JSON output: got %q want %q", got, want)
	}
	if got, want := payload.Policy.Hash, loader.compiled.Hash; got != want {
		t.Fatalf("unexpected policy hash in JSON output: got %v want %q", got, want)
	}
}

func TestPolicyValidateCommandRunText(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	loader := &policyValidateLoader{
		compiled: &policy.CompiledPolicy{Hash: "policy-hash-text"},
		source:   "/repo/cleanroom.yaml",
	}

	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err := (&PolicyValidateCommand{Chdir: "subdir"}).Run(&runtimeContext{
		CWD:    baseDir,
		Stdout: stdout,
		Loader: loader,
	})
	if err != nil {
		t.Fatalf("PolicyValidateCommand.Run returned error: %v", err)
	}
	if got, want := loader.loadedCWD, filepath.Join(baseDir, "subdir"); got != want {
		t.Fatalf("unexpected cwd passed to loader: got %q want %q", got, want)
	}

	output := readStdout()
	assertContainsAll(t, output,
		"policy valid: "+loader.source,
		"policy hash: "+loader.compiled.Hash,
	)
}

func TestPolicyValidateCommandRunTextUsesANSIWhenForced(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")

	loader := &policyValidateLoader{
		compiled: &policy.CompiledPolicy{Hash: "policy-hash-text"},
		source:   "/repo/cleanroom.yaml",
	}

	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err := (&PolicyValidateCommand{}).Run(&runtimeContext{
		CWD:    t.TempDir(),
		Stdout: stdout,
		Loader: loader,
	})
	if err != nil {
		t.Fatalf("PolicyValidateCommand.Run returned error: %v", err)
	}

	output := readStdout()
	plain := stripANSI(output)
	if !strings.Contains(output, "\x1b[") {
		t.Fatalf("expected ANSI escapes in color output: %q", output)
	}
	assertContainsAll(t, plain,
		"policy valid: "+loader.source,
		"policy hash: "+loader.compiled.Hash,
	)
}
