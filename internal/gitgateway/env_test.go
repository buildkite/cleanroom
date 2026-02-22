package gitgateway

import (
	"reflect"
	"testing"
)

func TestBuildScopedGitConfigEnv(t *testing.T) {
	t.Parallel()

	env := BuildScopedGitConfigEnv([]RewriteRule{{
		BaseURL:   "http://127.0.0.1:17080/git/github.com/",
		InsteadOf: "https://github.com/",
	}})

	want := []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url.http://127.0.0.1:17080/git/github.com/.insteadof",
		"GIT_CONFIG_VALUE_0=https://github.com/",
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("unexpected env: got %v want %v", env, want)
	}
}

func TestBuildScopedGitConfigEnvEmpty(t *testing.T) {
	t.Parallel()

	env := BuildScopedGitConfigEnv(nil)
	if env != nil {
		t.Fatalf("expected nil env for empty rules, got %v", env)
	}
}
