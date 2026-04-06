package repositorycheckout

import (
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
	if !strings.Contains(joined, "cd '/tmp/work tree' && exec 'printf' '%s' 'it'\"'\"'s alive'") {
		t.Fatalf("expected wrapped workdir command to quote args, got %q", joined)
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
