package cli

import (
	"errors"
	"fmt"
	"strings"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/repositorybundle"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
)

type workspaceCopyFlags struct {
	CopyIn bool `name:"copy-in" help:"Copy local workspace changes into the sandbox workspace before running"`
}

func (f workspaceCopyFlags) validate(_ string, fromSnapshot string, repositoryOverride repositoryOverrideFlags) error {
	if !f.CopyIn {
		return nil
	}
	switch {
	case strings.TrimSpace(fromSnapshot) != "":
		return errors.New("--copy-in cannot be used with --from")
	case repositoryOverride.hasRepositoryOverride():
		return errors.New("--copy-in cannot be used with --repo-url or --repo-commit")
	default:
		return nil
	}
}

func validateTopLevelWorkspaceCopyTransport(repository *resolvedRepositoryCheckout, copyWorkspace bool) error {
	if !copyWorkspace || repository != nil {
		return nil
	}
	return errors.New("--copy-in for non-Git workspaces cannot be used while creating a sandbox yet; create the sandbox first, then run cleanroom workspace copy-in <sandbox-id>")
}

type repositoryLocalChanges struct {
	Changeset    *cleanroomv1.RepositoryChangeset
	CommitBundle *cleanroomv1.RepositoryCommitBundle
	Files        []repositorychangeset.File
}

func resolveRepositoryLocalChanges(repository *resolvedRepositoryCheckout, copyWorkspace bool) (repositoryLocalChanges, error) {
	if !copyWorkspace {
		return repositoryLocalChanges{}, nil
	}
	if repository == nil {
		return repositoryLocalChanges{}, nil
	}
	if strings.TrimSpace(repository.RootDir) == "" {
		return repositoryLocalChanges{}, errors.New("--copy-in requires a local repository checkout")
	}
	if strings.TrimSpace(repository.RemoteName) == "" {
		return repositoryLocalChanges{}, errors.New("--copy-in requires a named repository remote")
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
		out.Files = append([]repositorychangeset.File(nil), changeset.Files...)
		out.Changeset = changeset.ToProto()
	}
	return out, nil
}
