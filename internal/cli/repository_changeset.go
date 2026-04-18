package cli

import (
	"errors"
	"fmt"
	"strings"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
)

type repositoryChangesetFlags struct {
	IncludeLocalChanges bool `name:"include-local-changes" help:"Package local uncommitted changes into a reproducible changeset and apply them after exact repository checkout"`
}

func (f repositoryChangesetFlags) validate(existingSandboxID, fromSnapshot string, repositoryOverride repositoryOverrideFlags) error {
	if !f.IncludeLocalChanges {
		return nil
	}
	switch {
	case strings.TrimSpace(existingSandboxID) != "":
		return errors.New("--include-local-changes cannot be used with --in")
	case strings.TrimSpace(fromSnapshot) != "":
		return errors.New("--include-local-changes cannot be used with --from")
	case repositoryOverride.hasRepositoryOverride():
		return errors.New("--include-local-changes cannot be used with --repo-url or --repo-commit")
	default:
		return nil
	}
}

func resolveRepositoryChangeset(repository *resolvedRepositoryCheckout, includeLocalChanges bool) (*cleanroomv1.RepositoryChangeset, error) {
	if !includeLocalChanges {
		return nil, nil
	}
	if repository == nil {
		return nil, errors.New("--include-local-changes requires a repository-aware top-level command")
	}
	if strings.TrimSpace(repository.RootDir) == "" {
		return nil, errors.New("--include-local-changes requires a local repository checkout")
	}

	changeset, err := repositorychangeset.BuildFromWorkingTree(repository.RootDir, toRepositoryCheckout(repository))
	if err != nil {
		return nil, fmt.Errorf("package local repository changes: %w", err)
	}
	if changeset == nil {
		return nil, nil
	}
	return changeset.ToProto(), nil
}
