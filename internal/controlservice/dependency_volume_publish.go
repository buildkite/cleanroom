package controlservice

import (
	"context"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

var dependencyBlockVolumePublishPhase = blockVolumePublishPhase{
	StageName:               dependencyVolumeStageName,
	PublishWarning:          "publish dependency block-volume caches",
	PersistWarning:          "persist dependency block-volume cache metadata",
	RollbackWarning:         "rollback dependency block-volume cache snapshots",
	RollbackSnapshotWarning: "rollback dependency block-volume cache snapshot",
	PublishedMessage:        "dependency block-volume cache published",
	LogWarning: func(s *Service, message, sandboxID string, err error) {
		s.logDependencyStageWarning(message, sandboxID, err)
	},
}

func (s *Service) maybePublishDependencyBlockVolumeCaches(
	ctx context.Context,
	adapter backend.CacheOutputVolumeSnapshottingAdapter,
	sandboxID, backendName string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	plan dependencyBlockVolumePlan,
) {
	s.maybePublishBlockVolumeCaches(ctx, adapter, sandboxID, backendName, compiled, firecrackerCfg, repository, changeset, dependencyBlockVolumePublishPhase, dependencyBlockVolumePublishBlocks(plan))
}

func dependencyBlockVolumePublishBlocks(plan dependencyBlockVolumePlan) []blockVolumePublishBlock {
	blocks := make([]blockVolumePublishBlock, 0, len(plan.Blocks))
	for _, block := range plan.Blocks {
		blocks = append(blocks, blockVolumePublishBlock{
			BlockName:               block.BlockName,
			Outputs:                 block.Outputs,
			CacheKey:                block.CacheKey,
			CommandDigest:           block.CommandDigest,
			EnvDigest:               block.EnvDigest,
			InputManifestDigest:     block.InputManifestDigest,
			NormalizedOutputsDigest: block.NormalizedOutputsDigest,
			ProducerVersion:         block.ProducerVersion,
			CacheHit:                block.CacheHit,
		})
	}
	return blocks
}
