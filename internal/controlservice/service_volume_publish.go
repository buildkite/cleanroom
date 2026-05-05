package controlservice

import (
	"context"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

var serviceBlockVolumePublishPhase = blockVolumePublishPhase{
	StageName:               serviceVolumeStageName,
	PublishWarning:          "publish service block-volume caches",
	PersistWarning:          "persist service block-volume cache metadata",
	RollbackWarning:         "rollback service block-volume cache snapshots",
	RollbackSnapshotWarning: "rollback service block-volume cache snapshot",
	PublishedMessage:        "service block-volume cache published",
	LogWarning: func(s *Service, message, sandboxID string, err error) {
		s.logServicesStageWarning(message, sandboxID, err)
	},
}

func (s *Service) maybePublishServiceBlockVolumeCaches(
	ctx context.Context,
	adapter backend.CacheOutputVolumeSnapshottingAdapter,
	sandboxID, backendName string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	plan serviceBlockVolumePlan,
) {
	s.maybePublishBlockVolumeCaches(ctx, adapter, sandboxID, backendName, compiled, firecrackerCfg, repository, changeset, serviceBlockVolumePublishPhase, serviceBlockVolumePublishBlocks(plan))
}

func serviceBlockVolumePublishBlocks(plan serviceBlockVolumePlan) []blockVolumePublishBlock {
	blocks := make([]blockVolumePublishBlock, 0, len(plan.Blocks))
	for _, block := range plan.Blocks {
		blocks = append(blocks, blockVolumePublishBlockFromPlanBlock(blockVolumeBlockPlan(block)))
	}
	return blocks
}
