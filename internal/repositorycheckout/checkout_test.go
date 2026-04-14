package repositorycheckout

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildBootstrapCommandUsesCurrentBranchWhenProvided(t *testing.T) {
	command := BuildBootstrapCommand(&Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      "0123456789abcdef0123456789abcdef01234567",
		DestinationDir: "/workspace",
		Branch:         "feature/console-branch",
	})

	joined := strings.Join(command, " ")
	if !strings.Contains(joined, "branch='feature/console-branch'") {
		t.Fatalf("expected bootstrap command to export branch name, got %q", joined)
	}
	if !strings.Contains(joined, "git -C \"$dest\" checkout -B \"$branch\" \"$commit\"") {
		t.Fatalf("expected bootstrap command to preserve branch name, got %q", joined)
	}
	if strings.Contains(joined, "checkout --detach") {
		t.Fatalf("expected bootstrap command to avoid detached checkout when branch is provided, got %q", joined)
	}
}

func TestBuildBootstrapCommandDetachesHeadWithoutBranch(t *testing.T) {
	command := BuildBootstrapCommand(&Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      "0123456789abcdef0123456789abcdef01234567",
		DestinationDir: "/workspace",
	})

	joined := strings.Join(command, " ")
	if !strings.Contains(joined, "git -C \"$dest\" checkout --detach \"$commit\"") {
		t.Fatalf("expected bootstrap command to detach HEAD without branch, got %q", joined)
	}
}

func TestBuildBootstrapCommandAllowsExistingEmptyDestination(t *testing.T) {
	command := BuildBootstrapCommand(&Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      "0123456789abcdef0123456789abcdef01234567",
		DestinationDir: "/workspace",
	})

	joined := strings.Join(command, " ")
	if strings.Contains(joined, `if [ -e "$dest" ]; then echo "repository destination already exists: $dest" >&2; exit 1; fi`) {
		t.Fatalf("expected bootstrap command to allow existing empty destination, got %q", joined)
	}
	if !strings.Contains(joined, `if [ -e "$dest" ] && [ ! -d "$dest" ]; then`) {
		t.Fatalf("expected bootstrap command to reject non-directory destinations explicitly, got %q", joined)
	}
	if !strings.Contains(joined, `if [ -d "$dest" ] && [ -n "$(ls -A "$dest")" ]; then`) {
		t.Fatalf("expected bootstrap command to reject only non-empty directories, got %q", joined)
	}
}

func TestNormalizeRemoteURLPreservesIPv6Brackets(t *testing.T) {
	checkout := &Checkout{
		RemoteURL:      "https://[2001:db8::1]/buildkite/cleanroom.git",
		CommitSHA:      "0123456789abcdef0123456789abcdef01234567",
		DestinationDir: "/workspace",
	}

	host, err := checkout.NormalizeRemoteURL()
	if err != nil {
		t.Fatalf("NormalizeRemoteURL returned error: %v", err)
	}
	if got, want := host, "2001:db8::1"; got != want {
		t.Fatalf("unexpected host: got %q want %q", got, want)
	}
	if got, want := checkout.RemoteURL, "https://[2001:db8::1]/buildkite/cleanroom.git"; got != want {
		t.Fatalf("unexpected normalized URL: got %q want %q", got, want)
	}
}

func TestCanonicalizeRemoteURLStripsUserInfo(t *testing.T) {
	gotURL, gotHost, err := CanonicalizeRemoteURL("https://token@github.com/buildkite/cleanroom.git")
	if err != nil {
		t.Fatalf("CanonicalizeRemoteURL returned error: %v", err)
	}
	if got, want := gotURL, "https://github.com/buildkite/cleanroom.git"; got != want {
		t.Fatalf("unexpected canonical URL: got %q want %q", got, want)
	}
	if got, want := gotHost, "github.com"; got != want {
		t.Fatalf("unexpected host: got %q want %q", got, want)
	}
}

func TestCanonicalizeRemoteURLAllowsExplicitDefaultSSHPort(t *testing.T) {
	gotURL, gotHost, err := CanonicalizeRemoteURL("ssh://git@github.com:22/buildkite/cleanroom.git")
	if err != nil {
		t.Fatalf("CanonicalizeRemoteURL returned error: %v", err)
	}
	if got, want := gotURL, "https://github.com/buildkite/cleanroom.git"; got != want {
		t.Fatalf("unexpected canonical URL: got %q want %q", got, want)
	}
	if got, want := gotHost, "github.com"; got != want {
		t.Fatalf("unexpected host: got %q want %q", got, want)
	}
}

func TestCanonicalizeRemoteURLRejectsMalformedHTTPSRemote(t *testing.T) {
	_, _, err := CanonicalizeRemoteURL("https://token@github.com:bad/org/repo.git")
	if err == nil {
		t.Fatal("expected CanonicalizeRemoteURL to reject malformed https remote")
	}
	if !strings.Contains(err.Error(), "parse repository remote URL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCanonicalizeRemoteURLPreservesIPv6Brackets(t *testing.T) {
	gotURL, gotHost, err := CanonicalizeRemoteURL("https://[2001:db8::1]/buildkite/cleanroom.git")
	if err != nil {
		t.Fatalf("CanonicalizeRemoteURL returned error: %v", err)
	}
	if got, want := gotURL, "https://[2001:db8::1]/buildkite/cleanroom.git"; got != want {
		t.Fatalf("unexpected canonical URL: got %q want %q", got, want)
	}
	if got, want := gotHost, "2001:db8::1"; got != want {
		t.Fatalf("unexpected host: got %q want %q", got, want)
	}
}

func TestBuildBootstrapCommandIncludesSubmoduleUpdateWhenRequested(t *testing.T) {
	command := BuildBootstrapCommand(&Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      "0123456789abcdef0123456789abcdef01234567",
		DestinationDir: "/workspace",
		Submodules:     true,
	})

	joined := strings.Join(command, " ")
	if !strings.Contains(joined, `git -C "$dest" submodule update --init --recursive`) {
		t.Fatalf("expected bootstrap command to update submodules, got %q", joined)
	}
}

func TestBootstrapRecipeDigestTracksCheckoutBehavior(t *testing.T) {
	base := &Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      "0123456789abcdef0123456789abcdef01234567",
		DestinationDir: "/workspace",
	}
	if got := BootstrapRecipeDigest(base); got == "" {
		t.Fatal("expected non-empty bootstrap recipe digest")
	}

	withBranch := &Checkout{
		RemoteURL:      base.RemoteURL,
		CommitSHA:      base.CommitSHA,
		DestinationDir: base.DestinationDir,
		Branch:         "feature/cache-stage",
	}
	if BootstrapRecipeDigest(withBranch) == BootstrapRecipeDigest(base) {
		t.Fatal("expected bootstrap recipe digest to change when checkout mode changes")
	}

	withSubmodules := &Checkout{
		RemoteURL:      base.RemoteURL,
		CommitSHA:      base.CommitSHA,
		DestinationDir: base.DestinationDir,
		Submodules:     true,
	}
	if BootstrapRecipeDigest(withSubmodules) == BootstrapRecipeDigest(base) {
		t.Fatal("expected bootstrap recipe digest to change when submodule bootstrap changes")
	}
}

func TestValidateBootstrapRejectsMutableCommitRef(t *testing.T) {
	checkout := &Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      "main",
		DestinationDir: "/workspace",
	}

	err := checkout.ValidateBootstrap()
	if err == nil {
		t.Fatal("expected ValidateBootstrap to reject mutable commit refs")
	}
	if !strings.Contains(err.Error(), "full 40-character hexadecimal commit SHA") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWrapCommandInWorkdirQuotesDestinationAndArguments(t *testing.T) {
	command := WrapCommandInWorkdir([]string{"printf", "%s", "it's alive"}, &Checkout{
		DestinationDir: "/tmp/work tree",
	})

	joined := strings.Join(command, " ")
	if !strings.Contains(joined, "dest='/tmp/work tree'") {
		t.Fatalf("expected wrapped workdir command to quote destination, got %q", joined)
	}
	if !strings.Contains(joined, `exec 'printf' '%s' 'it'"'"'s alive'`) {
		t.Fatalf("expected wrapped workdir command to quote args, got %q", joined)
	}
}

func TestWrapCommandInWorkdirRunsInRepositoryDirectory(t *testing.T) {
	repoDir := t.TempDir()
	outputPath := filepath.Join(t.TempDir(), "env")
	command := WrapCommandInWorkdir([]string{"sh", "-lc", "printf '%s\\n%s\\n%s\\n' \"$PWD\" \"$MISE_TRUSTED_CONFIG_PATHS\" \"$MISE_YES\" > " + shellQuote(outputPath)}, &Checkout{
		DestinationDir: repoDir,
	})

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = envWithout(os.Environ(), "MISE_TRUSTED_CONFIG_PATHS", "MISE_YES")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapped command failed: %v\n%s", err, string(out))
	}

	outputBytes, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(outputBytes)), "\n")
	if got, want := len(lines), 3; got != want {
		t.Fatalf("unexpected output line count: got %d want %d (%q)", got, want, string(outputBytes))
	}
	if got, want := strings.TrimSpace(lines[0]), repoDir; got != want {
		t.Fatalf("unexpected working directory: got %q want %q", got, want)
	}
	if got, want := strings.TrimSpace(lines[1]), repoDir; got != want {
		t.Fatalf("unexpected trusted config paths: got %q want %q", got, want)
	}
	if got, want := strings.TrimSpace(lines[2]), "1"; got != want {
		t.Fatalf("unexpected MISE_YES default: got %q want %q", got, want)
	}
}

func TestNormalizeCommandReturnsDetachedCopyWithoutSeparator(t *testing.T) {
	original := []string{"--", "sh", "-lc", "pwd"}
	normalized := NormalizeCommand(original)
	original[1] = "mutated"

	if got, want := strings.Join(normalized, " "), "sh -lc pwd"; got != want {
		t.Fatalf("unexpected normalized command: got %q want %q", got, want)
	}
}

func TestShellJoinQuotesSingleQuotes(t *testing.T) {
	got := shellJoin([]string{"echo", "it's"})
	if want := `'echo' 'it'"'"'s'`; got != want {
		t.Fatalf("unexpected shell join: got %q want %q", got, want)
	}
}

func TestWrapCommandWithBootstrapNormalizesPassthroughWithoutCheckout(t *testing.T) {
	command := WrapCommandWithBootstrap([]string{"--", "echo", "ok"}, nil)
	if got, want := strings.Join(command, " "), "echo ok"; got != want {
		t.Fatalf("unexpected normalized command: got %q want %q", got, want)
	}
}

func TestWrapCommandWithBootstrapIncludesRepositoryWorkdirExecution(t *testing.T) {
	command := WrapCommandWithBootstrap([]string{"sh", "-lc", "pwd"}, &Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      "0123456789abcdef0123456789abcdef01234567",
		DestinationDir: "/workspace",
	})

	joined := strings.Join(command, " ")
	if !strings.Contains(joined, `cd "$dest"`) {
		t.Fatalf("expected bootstrap wrapper to enter repository workdir, got %q", joined)
	}
	if !strings.Contains(joined, `exec 'sh' '-lc' 'pwd'`) {
		t.Fatalf("expected bootstrap wrapper to execute the requested command, got %q", joined)
	}
}

func TestWrapCommandInWorkdirFailsFastWhenWorkdirSetupFails(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "ran")
	command := WrapCommandInWorkdir([]string{"sh", "-lc", "touch " + shellQuote(outputPath)}, &Checkout{
		DestinationDir: filepath.Join(t.TempDir(), "missing"),
	})

	cmd := exec.Command(command[0], command[1:]...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected wrapped command to fail when cd fails; output=%s", string(out))
	}
	if _, statErr := os.Stat(outputPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected payload not to run when workdir setup fails, stat error=%v", statErr)
	}
}

func envWithout(env []string, keys ...string) []string {
	blocked := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		blocked[key] = struct{}{}
	}

	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, blockedKey := blocked[key]; blockedKey {
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	return filtered
}
