package cli

import (
	"errors"
	"fmt"
	"strings"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
)

type workspaceCopyFlags struct {
	Copy bool `name:"copy" help:"Copy local workspace changes into the sandbox workspace before running"`
}

func (f workspaceCopyFlags) validate(_ string, fromSnapshot string, repositoryOverride repositoryOverrideFlags) error {
	if !f.Copy {
		return nil
	}
	switch {
	case strings.TrimSpace(fromSnapshot) != "":
		return errors.New("--copy cannot be used with --from")
	case repositoryOverride.hasRepositoryOverride():
		return errors.New("--copy cannot be used with --repo-url or --repo-commit")
	default:
		return nil
	}
}

func resolveRepositoryChangeset(repository *resolvedRepositoryCheckout, copyWorkspace bool) (*cleanroomv1.RepositoryChangeset, error) {
	if !copyWorkspace {
		return nil, nil
	}
	if repository == nil {
		return nil, nil
	}
	if strings.TrimSpace(repository.RootDir) == "" {
		return nil, errors.New("--copy requires a local repository checkout")
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
