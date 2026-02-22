package firecracker

import (
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

func TestBuildGitScopedEnvDisabled(t *testing.T) {
	t.Parallel()

	env, err := buildGitScopedEnv(&policy.CompiledPolicy{}, "http://127.0.0.1:17080", "")
	if err != nil {
		t.Fatalf("buildGitScopedEnv returned error: %v", err)
	}
	if env != nil {
		t.Fatalf("expected nil env when git policy is disabled, got %v", env)
	}
}

func TestBuildGitScopedEnvEnabled(t *testing.T) {
	t.Parallel()

	env, err := buildGitScopedEnv(&policy.CompiledPolicy{
		Git: &policy.GitPolicy{
			Enabled:      true,
			AllowedHosts: []string{"github.com"},
		},
	}, "http://127.0.0.1:17080", "scope-token")
	if err != nil {
		t.Fatalf("buildGitScopedEnv returned error: %v", err)
	}
	if len(env) != 3 {
		t.Fatalf("unexpected git env count: got %d want 3", len(env))
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GIT_CONFIG_COUNT=1") {
		t.Fatalf("expected git config count in env, got:\n%s", joined)
	}
	if !strings.Contains(joined, "https://github.com/") {
		t.Fatalf("expected github rewrite in env, got:\n%s", joined)
	}
	if !strings.Contains(joined, "/git/scope-token/github.com/") {
		t.Fatalf("expected scoped github rewrite in env, got:\n%s", joined)
	}
}

func TestBuildGitScopedEnvErrorsWhenRelayURLMissing(t *testing.T) {
	t.Parallel()

	_, err := buildGitScopedEnv(&policy.CompiledPolicy{
		Git: &policy.GitPolicy{
			Enabled:      true,
			AllowedHosts: []string{"github.com"},
		},
	}, "", "")
	if err == nil {
		t.Fatal("expected error when relay URL is missing")
	}
}
