package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

const testDigestRef = "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestImageSetRefUpdatesPrimaryPolicy(t *testing.T) {
	dir := t.TempDir()
	policyPath := filepath.Join(dir, policy.PrimaryPolicyPath)
	if err := os.WriteFile(policyPath, []byte("version: 1\nsandbox:\n  image:\n    ref: docker.io/library/alpine@sha256:25109184c71bdad752c8312a8623239686a9a2071e8825f20acb8f2198c3f659\n"), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	stdout, readStdout := makeStdoutCapture(t)
	cmd := &ImageSetRefCommand{Ref: testDigestRef}

	if err := cmd.Run(&runtimeContext{CWD: dir, Stdout: stdout}); err != nil {
		t.Fatalf("run set-ref: %v", err)
	}

	raw, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatalf("read policy: %v", err)
	}
	if !strings.Contains(string(raw), "ref: "+testDigestRef) {
		t.Fatalf("policy missing updated ref, got:\n%s", raw)
	}

	output := readStdout()
	if !strings.Contains(output, "updated sandbox.image.ref") {
		t.Fatalf("expected status output, got %q", output)
	}
}

func TestImageSetRefFallsBackToBuildkitePolicy(t *testing.T) {
	dir := t.TempDir()
	fallbackDir := filepath.Join(dir, ".buildkite")
	if err := os.MkdirAll(fallbackDir, 0o755); err != nil {
		t.Fatalf("mkdir fallback dir: %v", err)
	}
	fallbackPath := filepath.Join(fallbackDir, "cleanroom.yaml")
	if err := os.WriteFile(fallbackPath, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatalf("write fallback policy: %v", err)
	}

	stdout, _ := makeStdoutCapture(t)
	cmd := &ImageSetRefCommand{Ref: testDigestRef}

	if err := cmd.Run(&runtimeContext{CWD: dir, Stdout: stdout}); err != nil {
		t.Fatalf("run set-ref: %v", err)
	}

	raw, err := os.ReadFile(fallbackPath)
	if err != nil {
		t.Fatalf("read fallback policy: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "sandbox:") || !strings.Contains(content, "ref: "+testDigestRef) {
		t.Fatalf("fallback policy missing sandbox.image.ref, got:\n%s", content)
	}
}

func TestImageSetRefRejectsNonDigestRef(t *testing.T) {
	dir := t.TempDir()
	policyPath := filepath.Join(dir, policy.PrimaryPolicyPath)
	if err := os.WriteFile(policyPath, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	stdout, _ := makeStdoutCapture(t)
	cmd := &ImageSetRefCommand{Ref: "ghcr.io/buildkite/cleanroom-base/alpine:latest"}

	err := cmd.Run(&runtimeContext{CWD: dir, Stdout: stdout})
	if err == nil {
		t.Fatal("expected set-ref error for non-digest ref")
	}
	if !strings.Contains(err.Error(), "digest-pinned") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func makeStdoutCapture(t *testing.T) (*os.File, func() string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout-*.txt")
	if err != nil {
		t.Fatalf("create stdout capture: %v", err)
	}
	return f, func() string {
		if err := f.Sync(); err != nil {
			t.Fatalf("sync stdout capture: %v", err)
		}
		if _, err := f.Seek(0, 0); err != nil {
			t.Fatalf("seek stdout capture: %v", err)
		}
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatalf("read stdout capture: %v", err)
		}
		return string(b)
	}
}
