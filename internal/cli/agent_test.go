package cli

import (
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

func TestAgentShellCommandUsesStringConfig(t *testing.T) {
	script, err := agentShellCommand("codex", []string{"--", "exec", "fix $PATH"}, map[string]runtimeconfig.Agent{
		"codex": {
			Command: "mise exec -- codex",
			Test:    "mise exec -- codex --version >/dev/null 2>&1",
			Install: "mise use -g npm:@openai/codex",
		},
	})
	if err != nil {
		t.Fatalf("agentShellCommand returned error: %v", err)
	}
	for _, want := range []string{
		"if ! (mise exec -- codex --version >/dev/null 2>&1); then",
		"mise use -g npm:@openai/codex",
		"exec mise exec -- codex 'exec' 'fix $PATH'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("expected script to contain %q:\n%s", want, script)
		}
	}
}

func TestAgentShellCommandQuotesSingleQuotes(t *testing.T) {
	script, err := agentShellCommand("codex", []string{"it's broken"}, nil)
	if err != nil {
		t.Fatalf("agentShellCommand returned error: %v", err)
	}
	if want := "exec codex 'it'\"'\"'s broken'"; !strings.Contains(script, want) {
		t.Fatalf("expected shell-quoted argument %q in script:\n%s", want, script)
	}
}
