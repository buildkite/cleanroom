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
