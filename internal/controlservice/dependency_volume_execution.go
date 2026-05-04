package controlservice

import (
	"context"
	"fmt"
	"path/filepath"
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

const dependencyInputProjectionRoot = "/run/cleanroom/input-projections/dependencies"

type dependencyBlockVolumePublishConfig struct {
	Adapter    backend.CacheOutputVolumeSnapshottingAdapter
	Backend    string
	Changeset  *repositorychangeset.Changeset
	Repository *repositorycheckout.Checkout
}

func (s *Service) bootstrapDependencyBlockVolumePlanInPersistentSandbox(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	publish dependencyBlockVolumePublishConfig,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	plan dependencyBlockVolumePlan,
	reporter CreateSandboxReporter,
) (bool, error) {
	if adapter == nil || compiled == nil || repository == nil || strings.TrimSpace(sandboxID) == "" || len(plan.Blocks) == 0 {
		return true, nil
	}

	publishable := true
	for _, block := range plan.Blocks {
		blockName := strings.TrimSpace(block.BlockName)
		if !publishable {
			// Later block-volume hits are unsafe once an earlier block needed
			// exact fallback; their keys assume prior blocks are represented by
			// declared output cache identities only.
			if _, err := s.bootstrapDependencyBlockVolumeFallback(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, block, true, reporter); err != nil {
				return publishable, fmt.Errorf("dependency block %q fallback: %w", blockName, err)
			}
			continue
		}
		if block.CacheHit {
			emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_DEPENDENCIES, "restoring dependency outputs: "+blockName)
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

		emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_DEPENDENCIES, "running dependency bootstrap: "+blockName)
		var result *backend.ExecutionResult
		if err := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.bootstrap_dependency_block", attrs, func(ctx context.Context) error {
			var err error
			result, err = s.bootstrapDependencyBlockVolumeBlock(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, block, reporter)
			return err
		}); err != nil {
			return publishable, fmt.Errorf("dependency block %q: %w", blockName, err)
		}
		if warning := blockVolumeEscapedWriteWarning("dependency", blockName, result); warning != "" {
			publishable = false
			emitCreateSandboxWarning(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_DEPENDENCIES, warning)
			s.logDependencyStageWarning("publish dependency block-volume caches", sandboxID, fmt.Errorf("%s", warning))
			if _, err := s.bootstrapDependencyBlockVolumeFallback(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, block, true, reporter); err != nil {
				return publishable, fmt.Errorf("dependency block %q fallback: %w", blockName, err)
			}
		}
		if publishable && publish.Adapter != nil {
			emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_PUBLISH_DEPENDENCY_STAGE_CACHE, "publishing dependency outputs: "+blockName)
			s.maybePublishDependencyBlockVolumeCaches(ctx, publish.Adapter, sandboxID, publish.Backend, compiled, firecrackerCfg, publish.Repository, publish.Changeset, dependencyBlockVolumePlan{
				ReuseNamespace: plan.ReuseNamespace,
				Blocks:         []dependencyBlockVolumeBlockPlan{block},
			})
		}
	}
	return publishable, nil
}

func (s *Service) bootstrapDependencyBlockVolumeFallback(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	block dependencyBlockVolumeBlockPlan,
	resetOutputs bool,
	reporter CreateSandboxReporter,
) (*backend.ExecutionResult, error) {
	if len(block.Command) == 0 {
		return nil, nil
	}
	sourceRoot := blockVolumeSourceRoot(repository)
	return s.runBlockVolumeExactFallback(
		ctx,
		adapter,
		sandboxID,
		compiled,
		firecrackerCfg,
		cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_DEPENDENCIES,
		policy.NetworkStageDependencies,
		"dependency",
		strings.TrimSpace(block.BlockName),
		sourceRoot,
		block.Command,
		stageBlockEnvList(block.Env),
		block.Outputs,
		resetOutputs,
		reporter,
	)
}

func (s *Service) bootstrapDependencyBlockVolumeBlock(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	block dependencyBlockVolumeBlockPlan,
	reporter CreateSandboxReporter,
) (*backend.ExecutionResult, error) {
	if len(block.Command) == 0 {
		return nil, nil
	}
	sourceRoot := strings.TrimSpace(repository.DestinationDir)
	if sourceRoot == "" {
		sourceRoot = "/workspace"
	}
	inputProjection := &backend.InputProjection{
		SourceRoot:          sourceRoot,
		TargetRoot:          filepath.Join(dependencyInputProjectionRoot, strings.TrimSpace(block.BlockName)),
		Files:               append([]string(nil), block.Inputs...),
		MountSourceReadOnly: true,
	}
	env := stageBlockEnvList(block.Env)
	_, result, stdout, stderr, err := s.runPersistentBootstrapCommandWithOptions(
		ctx,
		adapter,
		sandboxID,
		compiled,
		firecrackerCfg,
		cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_DEPENDENCIES,
		policy.NetworkStageDependencies,
		block.Command,
		env,
		nil,
		persistentSandboxCommandOptions{
			Dir:                     sourceRoot,
			ClosedEnv:               true,
			InputProjection:         inputProjection,
			CacheOutputFileCaptures: dependencyBlockVolumeFileCaptures(block),
			OverlayCapture:          dependencyBlockVolumeOverlayCapture(block),
		},
		reporter,
	)
	return result, persistentBootstrapCommandError(result, stdout, stderr, err, "dependency block bootstrap returned no result", "dependency block bootstrap failed with exit code %d")
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
