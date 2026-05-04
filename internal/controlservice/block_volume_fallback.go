package controlservice

import (
	"context"
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

func (s *Service) runBlockVolumeExactFallback(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	phase cleanroomv1.CreateSandboxPhase,
	networkStage policy.NetworkStage,
	stageName string,
	blockName string,
	sourceRoot string,
	command []string,
	env []string,
	outputs policy.StageBlockOutputs,
	resetOutputs bool,
	reporter CreateSandboxReporter,
) (*backend.ExecutionResult, error) {
	if resetOutputs {
		if err := s.resetBlockVolumeOutputs(ctx, adapter, sandboxID, compiled, firecrackerCfg, phase, networkStage, stageName, blockName, outputs, reporter); err != nil {
			return nil, err
		}
	}

	emitCreateSandboxMessage(reporter, phase, fmt.Sprintf("running %s bootstrap without block-volume cache: %s", stageName, blockName))
	_, result, stdout, stderr, err := s.runPersistentBootstrapCommandWithOptions(
		ctx,
		adapter,
		sandboxID,
		compiled,
		firecrackerCfg,
		phase,
		networkStage,
		command,
		env,
		nil,
		persistentSandboxCommandOptions{
			Dir:       sourceRoot,
			ClosedEnv: true,
		},
		reporter,
	)
	if fallbackErr := persistentBootstrapCommandError(
		result,
		stdout,
		stderr,
		err,
		fmt.Sprintf("%s block fallback returned no result", stageName),
		fmt.Sprintf("%s block fallback failed with exit code %%d", stageName),
	); fallbackErr != nil {
		return result, fallbackErr
	}
	return result, nil
}

func (s *Service) resetBlockVolumeOutputs(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	phase cleanroomv1.CreateSandboxPhase,
	networkStage policy.NetworkStage,
	stageName string,
	blockName string,
	outputs policy.StageBlockOutputs,
	reporter CreateSandboxReporter,
) error {
	command := blockVolumeOutputResetCommand(outputs)
	if len(command) == 0 {
		return nil
	}

	emitCreateSandboxMessage(reporter, phase, fmt.Sprintf("resetting %s outputs before block-volume fallback: %s", stageName, blockName))
	_, result, stdout, stderr, err := s.runPersistentBootstrapCommandWithOptions(
		ctx,
		adapter,
		sandboxID,
		compiled,
		firecrackerCfg,
		phase,
		networkStage,
		command,
		nil,
		nil,
		persistentSandboxCommandOptions{ClosedEnv: true},
		reporter,
	)
	if resetErr := persistentBootstrapCommandError(
		result,
		stdout,
		stderr,
		err,
		fmt.Sprintf("%s block output reset returned no result", stageName),
		fmt.Sprintf("%s block output reset failed with exit code %%d", stageName),
	); resetErr != nil {
		return fmt.Errorf("reset %s block %q outputs before fallback: %w", stageName, blockName, resetErr)
	}
	return nil
}

func blockVolumeOutputResetCommand(outputs policy.StageBlockOutputs) []string {
	var script strings.Builder
	script.WriteString("set -eu\n")
	wrote := false
	for _, path := range outputs.Files {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		script.WriteString("rm -rf -- " + blockVolumeShellQuote(path) + "\n")
		wrote = true
	}
	for _, path := range outputs.Dirs {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		quoted := blockVolumeShellQuote(path)
		script.WriteString("if [ -L " + quoted + " ]; then\n")
		script.WriteString("  rm -f -- " + quoted + "\n")
		script.WriteString("  mkdir -p -- " + quoted + "\n")
		script.WriteString("elif [ -d " + quoted + " ]; then\n")
		script.WriteString("  find " + quoted + " -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +\n")
		script.WriteString("else\n")
		script.WriteString("  rm -rf -- " + quoted + "\n")
		script.WriteString("  mkdir -p -- " + quoted + "\n")
		script.WriteString("fi\n")
		wrote = true
	}
	if !wrote {
		return nil
	}
	return []string{"sh", "-lc", script.String()}
}

func blockVolumeSourceRoot(repository *repositorycheckout.Checkout) string {
	if repository == nil {
		return "/workspace"
	}
	sourceRoot := strings.TrimSpace(repository.DestinationDir)
	if sourceRoot == "" {
		return "/workspace"
	}
	return sourceRoot
}

func blockVolumeShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
