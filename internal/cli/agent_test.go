package cli

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
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

func TestAgentShellCommandUsesDefaultAgentConfig(t *testing.T) {
	script, err := agentShellCommand("codex", []string{"exec", "summarize"}, nil)
	if err != nil {
		t.Fatalf("agentShellCommand returned error: %v", err)
	}
	for _, want := range []string{
		"command -v codex >/dev/null 2>&1 || command -v mise >/dev/null 2>&1",
		"exec sh -lc 'if command -v codex >/dev/null 2>&1; then exec codex \"$@\"; fi; exec env MISE_YES=1 MISE_TRUSTED_CONFIG_PATHS=/workspace mise --no-config exec -y nodejs@lts -- npm exec --yes --package @openai/codex@latest -- codex \"$@\"' sh 'exec' 'summarize'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("expected script to contain %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "mise use -g") {
		t.Fatalf("did not expect default agent install command:\n%s", script)
	}
}

func TestAgentShellCommandAllowsConfigToOverrideDefaultAgentCommand(t *testing.T) {
	script, err := agentShellCommand("codex", []string{"exec"}, map[string]runtimeconfig.Agent{
		"codex": {
			Command: "codex-nightly",
			Test:    "command -v codex-nightly >/dev/null 2>&1",
		},
	})
	if err != nil {
		t.Fatalf("agentShellCommand returned error: %v", err)
	}
	if want := "exec codex-nightly 'exec'"; !strings.Contains(script, want) {
		t.Fatalf("expected override command %q in script:\n%s", want, script)
	}
	if strings.Contains(script, "mise exec -- codex") {
		t.Fatalf("did not expect default command after override:\n%s", script)
	}
}

func TestAgentCredentialsUseDefaultAgentConfig(t *testing.T) {
	credentials := agentCredentials("codex", nil)
	if got, want := len(credentials), 2; got != want {
		t.Fatalf("unexpected credential count: got %d want %d", got, want)
	}
	if got, want := credentials[0], (runtimeconfig.AgentCredential{Source: "~/.codex/auth.json", Target: "~/.codex/auth.json"}); got != want {
		t.Fatalf("unexpected first credential: got %#v want %#v", got, want)
	}
	if got, want := credentials[1], (runtimeconfig.AgentCredential{Source: "~/.codex/config.toml", Target: "~/.codex/config.toml"}); got != want {
		t.Fatalf("unexpected second credential: got %#v want %#v", got, want)
	}
}

func TestAgentCredentialArchiveTrustsWorkspaceForCodexConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("model = \"gpt-5.5\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	archive, err := agentCredentialArchive("codex", map[string]runtimeconfig.Agent{
		"codex": {
			Credentials: []runtimeconfig.AgentCredential{
				{Source: configPath, Target: "~/.codex/config.toml"},
			},
		},
	})
	if err != nil {
		t.Fatalf("agentCredentialArchive returned error: %v", err)
	}

	entries := readTarEntries(t, archive)
	got := entries["root/.codex/config.toml"]
	for _, want := range []string{
		`model = "gpt-5.5"`,
		`[projects."/workspace"]`,
		`trust_level = "trusted"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected copied codex config to contain %q, got:\n%s", want, got)
		}
	}
}

func TestCodexConfigWithWorkspaceTrustDoesNotDuplicateExistingTrust(t *testing.T) {
	raw := []byte("model = \"gpt-5.5\"\n\n[projects.\"/workspace\"]\ntrust_level = \"trusted\"\n")
	got := codexConfigWithWorkspaceTrust(raw)
	if !bytes.Equal(got, raw) {
		t.Fatalf("expected existing workspace trust to be preserved without changes, got:\n%s", string(got))
	}
}

func TestAgentShellCommandQuotesSingleQuotes(t *testing.T) {
	script, err := agentShellCommand("custom", []string{"it's broken"}, nil)
	if err != nil {
		t.Fatalf("agentShellCommand returned error: %v", err)
	}
	if want := "exec custom 'it'\"'\"'s broken'"; !strings.Contains(script, want) {
		t.Fatalf("expected shell-quoted argument %q in script:\n%s", want, script)
	}
}

func readTarEntries(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(archive))
	entries := map[string]string{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar header: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar body: %v", err)
		}
		entries[header.Name] = string(body)
	}
	return entries
}
