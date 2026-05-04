package controlservice

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"go.opentelemetry.io/otel/attribute"
)

const serviceInputProjectionRoot = "/run/cleanroom/input-projections/services"

type serviceBlockVolumePublishConfig struct {
	Adapter    backend.CacheOutputVolumeSnapshottingAdapter
	Backend    string
	Changeset  *repositorychangeset.Changeset
	Repository *repositorycheckout.Checkout
}

func (s *Service) bootstrapServiceBlockVolumePlanInPersistentSandbox(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	publish serviceBlockVolumePublishConfig,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	plan serviceBlockVolumePlan,
	reporter CreateSandboxReporter,
) (bool, error) {
	if adapter == nil || compiled == nil || repository == nil || strings.TrimSpace(sandboxID) == "" || len(plan.Blocks) == 0 {
		return true, nil
	}

	publishable := true
	for _, block := range plan.Blocks {
		blockName := strings.TrimSpace(block.BlockName)
		if block.CacheHit {
			emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_SERVICES, "restoring service outputs: "+blockName)
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

		emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_SERVICES, "running service bootstrap: "+blockName)
		var result *backend.ExecutionResult
		if err := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.bootstrap_service_block", attrs, func(ctx context.Context) error {
			var err error
			result, err = s.bootstrapServiceBlockVolumeBlock(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, block, reporter)
			return err
		}); err != nil {
			return publishable, fmt.Errorf("service block %q: %w", blockName, err)
		}
		if warning := blockVolumeEscapedWriteWarning("service", blockName, result); warning != "" {
			publishable = false
			emitCreateSandboxWarning(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_SERVICES, warning)
			s.logServicesStageWarning("publish service block-volume caches", sandboxID, fmt.Errorf("%s", warning))
		}
		if publishable && publish.Adapter != nil {
			emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_PUBLISH_SERVICES_STAGE_CACHE, "publishing service outputs: "+blockName)
			s.maybePublishServiceBlockVolumeCaches(ctx, publish.Adapter, sandboxID, publish.Backend, compiled, firecrackerCfg, publish.Repository, publish.Changeset, serviceBlockVolumePlan{
				ReuseNamespace:       plan.ReuseNamespace,
				DependencyOutputKeys: append([]string(nil), plan.DependencyOutputKeys...),
				Blocks:               []serviceBlockVolumeBlockPlan{block},
			})
		}
	}
	return publishable, nil
}

func (s *Service) bootstrapServiceBlockVolumeBlock(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	block serviceBlockVolumeBlockPlan,
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
		TargetRoot:          filepath.Join(serviceInputProjectionRoot, strings.TrimSpace(block.BlockName)),
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
		cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_SERVICES,
		policy.NetworkStageServices,
		block.Command,
		env,
		nil,
		persistentSandboxCommandOptions{
			Dir:                     sourceRoot,
			ClosedEnv:               true,
			InputProjection:         inputProjection,
			CacheOutputFileCaptures: serviceBlockVolumeFileCaptures(block),
			OverlayCapture:          serviceBlockVolumeOverlayCapture(block),
		},
		reporter,
	)
	return result, persistentBootstrapCommandError(result, stdout, stderr, err, "service block bootstrap returned no result", "service block bootstrap failed with exit code %d")
}
