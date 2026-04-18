package controlservice

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

func hookCommand(command []string, repository *repositorycheckout.Checkout) []string {
	normalized := repositorycheckout.NormalizeCommand(command)
	if len(normalized) == 0 {
		return nil
	}
	if repository == nil {
		return normalized
	}
	return repositorycheckout.WrapCommandInWorkdir(normalized, repository)
}

func commandFailureError(result *backend.ExecutionResult, stdout, stderr string, err error, defaultMessage string) error {
	if err != nil {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = strings.TrimSpace(stdout)
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	if result == nil {
		return errors.New(defaultMessage)
	}
	if result.ExitCode != 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = strings.TrimSpace(stdout)
		}
		if msg == "" {
			msg = strings.TrimSpace(result.Message)
		}
		if msg == "" {
			msg = fmt.Sprintf("command failed with exit code %d", result.ExitCode)
		}
		return errors.New(msg)
	}
	return nil
}

func shouldRunPostDependenciesHookOnStoredRootFS(record storedRootFSRecord, compiled *policy.CompiledPolicy) bool {
	if compiled == nil || !compiled.Hooks.HasPostDependencies() {
		return false
	}
	switch strings.TrimSpace(record.Kind) {
	case "dependency stage cache":
		return true
	default:
		return false
	}
}

func (s *Service) runPostDependenciesHookInPersistentSandbox(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	reporter CreateSandboxReporter,
) error {
	if compiled == nil || !compiled.Hooks.HasPostDependencies() {
		return nil
	}

	command := hookCommand(compiled.Hooks.PostDependencies, repository)
	if len(command) == 0 {
		return nil
	}

	emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_RUN_POST_DEPENDENCIES_HOOK, "running post-dependencies hook")
	bootstrapExecutionID, result, stdout, stderr, err := s.runPersistentBootstrapCommand(
		ctx,
		adapter,
		sandboxID,
		compiled,
		firecrackerCfg,
		cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_RUN_POST_DEPENDENCIES_HOOK,
		command,
		reporter,
	)
	if s.Logger != nil {
		s.Logger.Debug("post-dependencies hook execution finished",
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

	return commandFailureError(result, stdout, stderr, err, "post-dependencies hook returned no result")
}
