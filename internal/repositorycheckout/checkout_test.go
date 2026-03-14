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
