package cli

import (
	"errors"
	"fmt"
	"strings"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/repositorybundle"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
)

type repositoryChangesetFlags struct {
	IncludeLocalChanges bool `name:"include-local-changes" help:"Package local-only commits plus uncommitted changes and apply them after exact repository checkout"`
}

type repositoryLocalChanges struct {
	Changeset    *cleanroomv1.RepositoryChangeset
	CommitBundle *cleanroomv1.RepositoryCommitBundle
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

func resolveRepositoryLocalChanges(repository *resolvedRepositoryCheckout, includeLocalChanges bool) (repositoryLocalChanges, error) {
	if !includeLocalChanges {
		return repositoryLocalChanges{}, nil
	}
	if repository == nil {
		return repositoryLocalChanges{}, errors.New("--include-local-changes requires a repository-aware top-level command")
	}
	if strings.TrimSpace(repository.RootDir) == "" {
		return repositoryLocalChanges{}, errors.New("--include-local-changes requires a local repository checkout")
	}
	if strings.TrimSpace(repository.RemoteName) == "" {
		return repositoryLocalChanges{}, errors.New("--include-local-changes requires a named repository remote")
	}

	commitBundle, err := repositorybundle.BuildFromRepository(repository.RootDir, repository.RemoteName, toRepositoryCheckout(repository))
	if err != nil {
		return repositoryLocalChanges{}, fmt.Errorf("package local repository commits: %w", err)
	}

	changeset, err := repositorychangeset.BuildFromWorkingTree(repository.RootDir, toRepositoryCheckout(repository))
	if err != nil {
		return repositoryLocalChanges{}, fmt.Errorf("package local repository changes: %w", err)
	}
	out := repositoryLocalChanges{}
	if commitBundle != nil {
		out.CommitBundle = commitBundle.ToProto()
	}
	if changeset != nil {
		out.Changeset = changeset.ToProto()
	}
	return out, nil
}
