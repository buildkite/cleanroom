package controlservice

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/changesetstore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

func repositoryChangesetFromProto(proto *cleanroomv1.RepositoryChangeset) *repositorychangeset.Changeset {
	return repositorychangeset.FromProto(proto)
}

func validateRepositoryChangesetForCheckout(repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset) error {
	if changeset == nil {
		return nil
	}
	if repository == nil {
		return errors.New("repository changeset requires a repository checkout")
	}
	if err := changeset.ValidateForCheckout(repository); err != nil {
		return err
	}
	if err := changeset.ValidateContent(); err != nil {
		return err
	}
	return nil
}

func repositoryChangesetID(repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset) string {
	return changesetstore.RecordID(repository, changeset)
}

func cacheRecordChangesetIDMatches(recordChangesetID string, repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset) bool {
	recordChangesetID = strings.TrimSpace(recordChangesetID)
	expectedChangesetID := repositoryChangesetID(repository, changeset)
	return recordChangesetID == expectedChangesetID
}

func (s *Service) persistRepositoryChangeset(ctx context.Context, repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset) (changesetstore.Record, error) {
	if changeset == nil {
		return changesetstore.Record{}, nil
	}
	if s.ChangesetStore == nil {
		return changesetstore.Record{}, nil
	}
	record, err := s.ChangesetStore.Put(ctx, repository, changeset)
	if err != nil {
		return changesetstore.Record{}, fmt.Errorf("persist repository changeset: %w", err)
	}
	if s.Logger != nil {
		s.Logger.Debug("repository changeset persisted",
			"changeset_id", record.ChangesetID,
			"changeset_digest", record.ChangesetDigest,
			"tree_digest", record.FinalTreeDigest,
		)
	}
	return record, nil
}

func persistentBootstrapCommandError(result *backend.ExecutionResult, stdout, stderr string, err error, missingResultMessage, exitCodeMessage string) error {
	if err != nil {
		message := strings.TrimSpace(stderr)
		if message == "" {
			message = strings.TrimSpace(stdout)
		}
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("%s", message)
	}
	if result == nil {
		return errors.New(missingResultMessage)
	}
	if result.ExitCode != 0 {
		message := strings.TrimSpace(stderr)
		if message == "" {
			message = strings.TrimSpace(stdout)
		}
		if message == "" {
			message = strings.TrimSpace(result.Message)
		}
		if message == "" {
			message = fmt.Sprintf(exitCodeMessage, result.ExitCode)
		}
		return errors.New(message)
	}
	return nil
}

func (s *Service) bootstrapRepositoryChangesetInPersistentSandbox(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	reporter CreateSandboxReporter,
) error {
	if changeset == nil {
		return nil
	}
	if s.Logger != nil {
		s.Logger.Debug("starting repository changeset apply",
			"sandbox_id", sandboxID,
			"destination_dir", repository.DestinationDir,
			"changeset_digest", changeset.Digest,
			"tree_digest", changeset.TreeDigest,
		)
	}

	bootstrapExecutionID, result, stdout, stderr, err := s.runPersistentBootstrapCommand(
		ctx,
		adapter,
		sandboxID,
		compiled,
		firecrackerCfg,
		cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_APPLY_REPOSITORY_CHANGESET,
		policy.NetworkStageWorkspace,
		repositorychangeset.ApplyCommand(repository, changeset),
		changeset.Patch,
		reporter,
	)
	if s.Logger != nil {
		s.Logger.Debug("repository changeset apply finished",
			"sandbox_id", sandboxID,
			"execution_id", bootstrapExecutionID,
			"exit_code", func() int {
				if result == nil {
					return -1
				}
				return result.ExitCode
			}(),
			"error", err,
		)
	}
	return persistentBootstrapCommandError(result, stdout, stderr, err, "repository changeset apply returned no result", "repository changeset apply failed with exit code %d")
}
