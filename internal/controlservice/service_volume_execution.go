package controlservice

import (
	"context"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

const serviceInputProjectionRoot = "/run/cleanroom/input-projections/services"

var serviceBlockVolumeExecutionPhase = blockVolumeExecutionPhase{
	StageName:           serviceVolumeStageName,
	StageLabel:          "service",
	InputProjectionRoot: serviceInputProjectionRoot,
	BootstrapPhase:      cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_SERVICES,
	PublishPhase:        cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_PUBLISH_SERVICES_STAGE_CACHE,
	NetworkStage:        policy.NetworkStageServices,
	CachePublishPhase:   serviceBlockVolumePublishPhase,
}

type serviceBlockVolumePublishConfig struct {
	Adapter            backend.CacheOutputVolumeSnapshottingAdapter
	Backend            string
	Changeset          *repositorychangeset.Changeset
	Repository         *repositorycheckout.Checkout
	ForceExactFallback bool
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
	return s.bootstrapBlockVolumePlanInPersistentSandbox(
		ctx,
		adapter,
		sandboxID,
		blockVolumeExecutionPublishConfig{
			Adapter:            publish.Adapter,
			Backend:            publish.Backend,
			Changeset:          publish.Changeset,
			Repository:         publish.Repository,
			ForceExactFallback: publish.ForceExactFallback,
		},
		compiled,
		firecrackerCfg,
		repository,
		serviceBlockVolumeExecutionPhase,
		serviceBlockVolumeExecutionBlocks(plan),
		reporter,
	)
}

func serviceBlockVolumeExecutionBlocks(plan serviceBlockVolumePlan) []blockVolumeBlockPlan {
	blocks := make([]blockVolumeBlockPlan, 0, len(plan.Blocks))
	for _, block := range plan.Blocks {
		blocks = append(blocks, blockVolumeBlockPlan(block))
	}
	return blocks
}
