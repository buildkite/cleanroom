package controlservice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachestore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

type dependencyBlockVolumePublishBlock struct {
	Block      dependencyBlockVolumeBlockPlan
	Spec       backend.CacheOutputVolumeSpec
	SnapshotID string
}

func (s *Service) maybePublishDependencyBlockVolumeCaches(
	ctx context.Context,
	snapshotAdapter backend.SnapshottingAdapter,
	adapter backend.Adapter,
	sandboxID, backendName string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	plan dependencyBlockVolumePlan,
	reporter CreateSandboxReporter,
) {
	if adapter == nil || compiled == nil || repository == nil || strings.TrimSpace(sandboxID) == "" || len(plan.Blocks) == 0 {
		return
	}
	if snapshotAdapter == nil || !snapshotOperationsEnabledForBackend(backendName, s.Config) {
		return
	}
	volumeSnapshotter, ok := adapter.(backend.CacheOutputVolumeSnapshotter)
	if !ok {
		s.logDependencyBlockVolumeWarning("publish dependency block output caches", sandboxID, errors.New("backend does not support cache output volume snapshots"))
		return
	}
	store, err := s.cacheStoreOrErr()
	if err != nil {
		s.logDependencyBlockVolumeWarning("publish dependency block output caches", sandboxID, err)
		return
	}

	blocks := make([]dependencyBlockVolumePublishBlock, 0, len(plan.Blocks))
	requests := make([]backend.CacheOutputVolumeSnapshotRequest, 0, len(plan.Blocks))
	for _, block := range plan.Blocks {
		blockName := strings.TrimSpace(block.BlockName)
		if block.CacheHit {
			continue
		}
		if len(block.Outputs.Files) > 0 {
			s.logDependencyBlockVolumeWarning("publish dependency block output cache", sandboxID, fmt.Errorf("dependency block %q declares file outputs, which are not captured yet", blockName))
			continue
		}
		if record, found, lookupErr := store.GetReady(ctx, dependencyVolumeStageName, block.CacheKey); lookupErr != nil {
			s.logDependencyBlockVolumeWarning("lookup dependency block output cache before publish", sandboxID, lookupErr)
			continue
		} else if found {
			s.logDependencyBlockVolumeAlreadyPublished(record)
			continue
		}

		spec, err := blockVolumeOutputSpec(blockVolumeOutputSpecBlock{
			Stage:     dependencyVolumeStageName,
			BlockName: block.BlockName,
			CacheKey:  block.CacheKey,
			Outputs:   block.Outputs,
		})
		if err != nil {
			s.logDependencyBlockVolumeWarning("prepare dependency block output cache publish", sandboxID, err)
			continue
		}
		snapshotID := newSnapshotID()
		blocks = append(blocks, dependencyBlockVolumePublishBlock{
			Block:      block,
			Spec:       spec,
			SnapshotID: snapshotID,
		})
		requests = append(requests, backend.CacheOutputVolumeSnapshotRequest{
			Stage:      dependencyVolumeStageName,
			BlockName:  block.BlockName,
			CacheKey:   block.CacheKey,
			VolumeID:   spec.VolumeID,
			SnapshotID: snapshotID,
		})
	}
	if len(requests) == 0 {
		return
	}

	snapshotCfg := withSnapshotDriver(backendName, firecrackerCfg, firecrackerCfg.Snapshots.Driver)
	result, err := volumeSnapshotter.SnapshotCacheOutputVolumes(ctx, backend.SnapshotCacheOutputVolumesRequest{
		SandboxID:         sandboxID,
		Volumes:           requests,
		FirecrackerConfig: snapshotCfg,
	})
	if err != nil {
		s.logDependencyBlockVolumeWarning("snapshot dependency block output volumes", sandboxID, err)
		return
	}
	if result == nil {
		s.logDependencyBlockVolumeWarning("snapshot dependency block output volumes", sandboxID, errors.New("backend returned no result"))
		return
	}
	snapshotsByVolumeID := make(map[string]backend.CacheOutputVolumeSnapshot, len(result.Volumes))
	for _, snapshot := range result.Volumes {
		volumeID := strings.TrimSpace(snapshot.VolumeID)
		if volumeID == "" {
			s.logDependencyBlockVolumeWarning("snapshot dependency block output volumes", sandboxID, errors.New("backend returned snapshot without volume id"))
			continue
		}
		snapshotsByVolumeID[volumeID] = snapshot
	}

	for _, publishBlock := range blocks {
		blockName := strings.TrimSpace(publishBlock.Block.BlockName)
		emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_PUBLISH_DEPENDENCY_STAGE_CACHE, "publishing dependency outputs: "+blockName)
		snapshot, ok := snapshotsByVolumeID[strings.TrimSpace(publishBlock.Spec.VolumeID)]
		if !ok {
			s.logDependencyBlockVolumeWarning("persist dependency block output cache metadata", sandboxID, fmt.Errorf("backend returned no snapshot for dependency block %q", blockName))
			continue
		}
		record, err := dependencyBlockVolumeRecordFromSnapshot(compiled, repository, changeset, publishBlock, snapshot, backendName, s.clock().Now())
		if err != nil {
			s.logDependencyBlockVolumeWarning("prepare dependency block output cache metadata", sandboxID, err)
			s.deleteDependencyBlockVolumeSnapshot(ctx, snapshotAdapter, snapshotCfg, snapshot)
			continue
		}
		if err := store.Create(ctx, record); err != nil {
			s.deleteDependencyBlockVolumeSnapshot(ctx, snapshotAdapter, snapshotCfg, snapshot)
			s.logDependencyBlockVolumeWarning("persist dependency block output cache metadata", sandboxID, err)
			continue
		}
		s.logDependencyBlockVolumePublished(record, sandboxID)
	}
}

func dependencyBlockVolumeRecordFromSnapshot(
	compiled *policy.CompiledPolicy,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	publishBlock dependencyBlockVolumePublishBlock,
	snapshot backend.CacheOutputVolumeSnapshot,
	backendName string,
	now time.Time,
) (cachestore.Record, error) {
	block := publishBlock.Block
	outputRecords := make([]cachestore.OutputRecord, 0, len(publishBlock.Spec.DirMappings))
	for _, mapping := range publishBlock.Spec.DirMappings {
		outputRecords = append(outputRecords, cachestore.OutputRecord{
			Kind:          "dir",
			Path:          strings.TrimSpace(mapping.GuestPath),
			VolumeSubpath: strings.TrimSpace(mapping.Subpath),
			StorageDriver: strings.TrimSpace(snapshot.StorageDriver),
			StorageRef:    strings.TrimSpace(snapshot.StorageRef),
			SnapshotRef:   strings.TrimSpace(snapshot.StorageRef),
		})
	}
	if reason := blockVolumeOutputRecordMissReason(block.Outputs, outputRecords); reason != "" {
		return cachestore.Record{}, fmt.Errorf("dependency block %q output records are invalid: %s", block.BlockName, reason)
	}
	outputManifestDigest, err := digestCanonicalJSON(outputRecords)
	if err != nil {
		return cachestore.Record{}, fmt.Errorf("digest dependency block %q output records: %w", block.BlockName, err)
	}

	storageDriver := strings.TrimSpace(snapshot.StorageDriver)
	storageRef := strings.TrimSpace(snapshot.StorageRef)
	if storageDriver == "" {
		return cachestore.Record{}, fmt.Errorf("dependency block %q snapshot missing storage driver", block.BlockName)
	}
	if storageRef == "" {
		return cachestore.Record{}, fmt.Errorf("dependency block %q snapshot missing storage ref", block.BlockName)
	}
	now = now.UTC()
	if now.IsZero() {
		return cachestore.Record{}, errors.New("dependency block volume publish time is zero")
	}
	record := cachestore.Record{
		CacheKey:                strings.TrimSpace(block.CacheKey),
		Stage:                   dependencyVolumeStageName,
		ReuseMode:               dependencyVolumeReuseMode,
		State:                   cacheStateReady,
		BackingSnapshotID:       strings.TrimSpace(snapshot.SnapshotID),
		Backend:                 strings.TrimSpace(backendName),
		PolicyHash:              compiled.Hash,
		Policy:                  compiled.ToProto(),
		Repository:              cloneRepositoryCheckout(normalizeRepositoryCheckoutForComparison(repository)).ToProto(),
		RepositoryHasChangeset:  changeset != nil,
		RepositoryChangesetID:   repositoryChangesetID(repository, changeset),
		ParentCacheKey:          strings.TrimSpace(block.PriorDependencyOutputKeysDigest),
		StorageDriver:           storageDriver,
		StorageRef:              storageRef,
		InputManifestDigest:     strings.TrimSpace(block.InputManifestDigest),
		CommandDigest:           strings.TrimSpace(block.CommandDigest),
		EnvDigest:               strings.TrimSpace(block.EnvDigest),
		NormalizedOutputsDigest: strings.TrimSpace(block.NormalizedOutputsDigest),
		OutputManifestDigest:    outputManifestDigest,
		OutputRecords:           outputRecords,
		CreatedAt:               now,
		LastUsedAt:              now,
		ProducerVersion:         strings.TrimSpace(block.ProducerVersion),
	}
	populateStageCacheRecordMetadata(&record, block.RuntimeBaseKey, &backend.SnapshotResult{
		StorageRef:         storageRef,
		StorageSizeBytes:   snapshot.StorageSizeBytes,
		ExclusiveSizeBytes: snapshot.ExclusiveSizeBytes,
		DriverMetadata:     snapshot.DriverMetadata,
	}, now)
	return record, nil
}

func (s *Service) deleteDependencyBlockVolumeSnapshot(ctx context.Context, snapshotAdapter backend.SnapshottingAdapter, cfg backend.FirecrackerConfig, snapshot backend.CacheOutputVolumeSnapshot) {
	if snapshotAdapter == nil {
		return
	}
	if err := snapshotAdapter.DeleteSnapshot(ctx, backend.DeleteSnapshotRequest{
		SnapshotID:        strings.TrimSpace(snapshot.SnapshotID),
		StorageRef:        strings.TrimSpace(snapshot.StorageRef),
		FirecrackerConfig: cfg,
	}); err != nil {
		s.logDependencyBlockVolumeWarning("rollback dependency block output cache after metadata failure", "", err)
	}
}

func (s *Service) logDependencyBlockVolumeAlreadyPublished(record cachestore.Record) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Debug("dependency block output cache already published",
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backend", strings.TrimSpace(record.Backend),
	)
}

func (s *Service) logDependencyBlockVolumePublished(record cachestore.Record, sandboxID string) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Info("dependency block output cache published",
		"sandbox_id", strings.TrimSpace(sandboxID),
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backend", strings.TrimSpace(record.Backend),
	)
}

func (s *Service) logDependencyBlockVolumeWarning(operation, sandboxID string, err error) {
	if s == nil || s.Logger == nil || err == nil {
		return
	}
	s.Logger.Warn(operation,
		"sandbox_id", strings.TrimSpace(sandboxID),
		"error", err,
	)
}
