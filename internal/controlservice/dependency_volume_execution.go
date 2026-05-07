package controlservice

import (
	"context"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

var dependencyBlockVolumeExecutionPhase = blockVolumeExecutionPhase{
	StageName:         dependencyVolumeStageName,
	StageLabel:        "dependency",
	BootstrapPhase:    cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_DEPENDENCIES,
	PublishPhase:      cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_PUBLISH_DEPENDENCY_STAGE_CACHE,
	NetworkStage:      policy.NetworkStageDependencies,
	CachePublishPhase: dependencyBlockVolumePublishPhase,
}

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
	return s.bootstrapBlockVolumePlanInPersistentSandbox(
		ctx,
		adapter,
		sandboxID,
		blockVolumeExecutionPublishConfig{
			Adapter:    publish.Adapter,
			Backend:    publish.Backend,
			Changeset:  publish.Changeset,
			Repository: publish.Repository,
		},
		compiled,
		firecrackerCfg,
		repository,
		dependencyBlockVolumeExecutionPhase,
		dependencyBlockVolumeExecutionBlocks(plan),
		reporter,
	)
}

func dependencyBlockVolumeExecutionBlocks(plan dependencyBlockVolumePlan) []blockVolumeBlockPlan {
	blocks := make([]blockVolumeBlockPlan, 0, len(plan.Blocks))
	for _, block := range plan.Blocks {
		blocks = append(blocks, blockVolumeBlockPlan(block))
	}
	return blocks
}
