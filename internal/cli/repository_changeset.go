package cli

import (
	"errors"
	"fmt"
	"strings"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/repositorybundle"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
)

type workspaceCopyInFlags struct {
	CopyIn bool `name:"copy-in" help:"Copy local workspace changes into the sandbox workspace before running"`
}

func (f workspaceCopyInFlags) validate(fromSnapshot string, repositoryOverride repositoryOverrideFlags) error {
	switch {
	case f.CopyIn && strings.TrimSpace(fromSnapshot) != "":
		return errors.New("--copy-in cannot be used with --from")
	case f.CopyIn && repositoryOverride.hasRepositoryOverride():
		return errors.New("--copy-in cannot be used with --repo-url or --repo-commit")
	default:
		return nil
	}
}

type workspaceCopyFlags struct {
	CopyIn  bool `name:"copy-in" help:"Copy local workspace changes into the sandbox workspace before running"`
	CopyOut bool `name:"copy-out" help:"Copy sandbox workspace changes back to the local workspace after running"`
	Sync    bool `name:"sync" help:"Equivalent to --copy-in --copy-out"`
}

func (f workspaceCopyFlags) copyIn() bool {
	return f.CopyIn || f.Sync
}

func (f workspaceCopyFlags) copyOut() bool {
	return f.CopyOut || f.Sync
}

func (f workspaceCopyFlags) validate(fromSnapshot string, repositoryOverride repositoryOverrideFlags) error {
	switch {
	case f.Sync && strings.TrimSpace(fromSnapshot) != "":
		return errors.New("--sync cannot be used with --from")
	case f.CopyIn && strings.TrimSpace(fromSnapshot) != "":
		return errors.New("--copy-in cannot be used with --from")
	case f.CopyOut && strings.TrimSpace(fromSnapshot) != "":
		return errors.New("--copy-out cannot be used with --from")
	case f.Sync && repositoryOverride.hasRepositoryOverride():
		return errors.New("--sync cannot be used with --repo-url or --repo-commit")
	case f.CopyIn && repositoryOverride.hasRepositoryOverride():
		return errors.New("--copy-in cannot be used with --repo-url or --repo-commit")
	case f.CopyOut && repositoryOverride.hasRepositoryOverride():
		return errors.New("--copy-out cannot be used with --repo-url or --repo-commit")
	default:
		return nil
	}
}

func validateTopLevelWorkspaceCopyTransport(repository *resolvedRepositoryCheckout, copyWorkspace bool) error {
	if !copyWorkspace || repository != nil {
		return nil
	}
	return errors.New("workspace copy requires a local Git repository checkout")
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
