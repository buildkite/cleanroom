package controlservice

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"go.opentelemetry.io/otel/attribute"
)

type blockVolumeExecutionPhase struct {
	StageName         string
	StageLabel        string
	BootstrapPhase    cleanroomv1.CreateSandboxPhase
	PublishPhase      cleanroomv1.CreateSandboxPhase
	NetworkStage      policy.NetworkStage
	CachePublishPhase blockVolumePublishPhase
}

type blockVolumeExecutionPublishConfig struct {
	Adapter            backend.CacheOutputVolumeSnapshottingAdapter
	Backend            string
	Changeset          *repositorychangeset.Changeset
	Repository         *repositorycheckout.Checkout
	ForceExactFallback bool
}

func (s *Service) bootstrapBlockVolumePlanInPersistentSandbox(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	publish blockVolumeExecutionPublishConfig,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	phase blockVolumeExecutionPhase,
	blocks []blockVolumeBlockPlan,
	reporter CreateSandboxReporter,
) (bool, error) {
	if adapter == nil || compiled == nil || repository == nil || strings.TrimSpace(sandboxID) == "" || len(blocks) == 0 {
		return true, nil
	}

	publishable := !publish.ForceExactFallback
	for _, block := range blocks {
		blockName := strings.TrimSpace(block.BlockName)
		if !publishable {
			// Later block-volume hits are unsafe once this phase, or a phase it
			// builds on, needed exact fallback.
			if _, err := s.bootstrapBlockVolumeFallback(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, phase, block, true, reporter); err != nil {
				return publishable, fmt.Errorf("%s block %q fallback: %w", phase.StageLabel, blockName, err)
			}
			continue
		}
		if block.CacheHit {
			emitCreateSandboxMessage(reporter, phase.BootstrapPhase, fmt.Sprintf("restoring %s outputs: %s", phase.StageLabel, blockName))
			continue
		}

		attrs := []attribute.KeyValue{
			attribute.String(observability.AttrBackend, adapter.Name()),
			attribute.String(observability.AttrSandboxID, sandboxID),
			attribute.String("cleanroom.cache.block", blockName),
			attribute.Int(observability.AttrCommandArgc, len(block.Command)),
		}
		if commitSHA := strings.TrimSpace(repository.CommitSHA); commitSHA != "" {
			attrs = append(attrs, attribute.String(observability.AttrRepositoryCommitSHA, commitSHA))
		}

		emitCreateSandboxMessage(reporter, phase.BootstrapPhase, fmt.Sprintf("running %s bootstrap: %s", phase.StageLabel, blockName))
		var result *backend.ExecutionResult
		if err := s.traceCreateSandboxPhase(ctx, fmt.Sprintf("cleanroom.sandbox.bootstrap_%s_block", phase.StageLabel), attrs, func(ctx context.Context) error {
			var err error
			result, err = s.bootstrapBlockVolumeBlock(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, phase, block, reporter)
			return err
		}); err != nil {
			return publishable, fmt.Errorf("%s block %q: %w", phase.StageLabel, blockName, err)
		}
		if warning := blockVolumeEscapedWriteWarning(phase.StageLabel, blockName, result); warning != "" {
			publishable = false
			emitCreateSandboxWarning(reporter, phase.BootstrapPhase, warning)
			phase.CachePublishPhase.warn(s, phase.CachePublishPhase.PublishWarning, sandboxID, fmt.Errorf("%s", warning))
			if _, err := s.bootstrapBlockVolumeFallback(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, phase, block, true, reporter); err != nil {
				return publishable, fmt.Errorf("%s block %q fallback: %w", phase.StageLabel, blockName, err)
			}
		}
		if publishable && publish.Adapter != nil {
			emitCreateSandboxMessage(reporter, phase.PublishPhase, fmt.Sprintf("publishing %s outputs: %s", phase.StageLabel, blockName))
			s.maybePublishBlockVolumeCaches(ctx, publish.Adapter, sandboxID, publish.Backend, compiled, firecrackerCfg, publish.Repository, publish.Changeset, phase.CachePublishPhase, []blockVolumePublishBlock{
				blockVolumePublishBlockFromPlanBlock(block),
			})
		}
	}
	return publishable, nil
}

func (s *Service) bootstrapBlockVolumeFallback(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	phase blockVolumeExecutionPhase,
	block blockVolumeBlockPlan,
	resetOutputs bool,
	reporter CreateSandboxReporter,
) (*backend.ExecutionResult, error) {
	if len(block.Command) == 0 {
		return nil, nil
	}
	return s.runBlockVolumeExactFallback(
		ctx,
		adapter,
		sandboxID,
		compiled,
		firecrackerCfg,
		phase.BootstrapPhase,
		phase.NetworkStage,
		phase.StageLabel,
		strings.TrimSpace(block.BlockName),
		blockVolumeSourceRoot(repository),
		block.Command,
		stageBlockEnvList(block.Env),
		block.Outputs,
		resetOutputs,
		reporter,
	)
}

func (s *Service) bootstrapBlockVolumeBlock(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	phase blockVolumeExecutionPhase,
	block blockVolumeBlockPlan,
	reporter CreateSandboxReporter,
) (*backend.ExecutionResult, error) {
	if len(block.Command) == 0 {
		return nil, nil
	}
	sourceRoot := blockVolumeSourceRoot(repository)
	env := stageBlockEnvList(block.Env)
	_, result, stdout, stderr, err := s.runPersistentBootstrapCommandWithOptions(
		ctx,
		adapter,
		sandboxID,
		compiled,
		firecrackerCfg,
		phase.BootstrapPhase,
		phase.NetworkStage,
		block.Command,
		env,
		nil,
		persistentSandboxCommandOptions{
			Dir:                     sourceRoot,
			ClosedEnv:               true,
			CacheOutputFileCaptures: blockVolumeFileCaptures(phase.StageName, block.CacheKey, block.Outputs),
			OverlayCapture:          blockVolumeOverlayCapture(phase.StageName, block.CacheKey, block.Outputs),
		},
		reporter,
	)
	return result, persistentBootstrapCommandError(
		result,
		stdout,
		stderr,
		err,
		fmt.Sprintf("%s block bootstrap returned no result", phase.StageLabel),
		fmt.Sprintf("%s block bootstrap failed with exit code %%d", phase.StageLabel),
	)
}

func stageBlockEnvList(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}
