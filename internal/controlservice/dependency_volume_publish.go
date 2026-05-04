package controlservice

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachestore"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

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
	if adapter == nil || compiled == nil || repository == nil || strings.TrimSpace(sandboxID) == "" || len(plan.Blocks) == 0 {
		return
	}
	store, err := s.cacheStoreOrErr()
	if err != nil {
		s.logDependencyStageWarning("publish dependency block-volume caches", sandboxID, err)
		return
	}

	missed := missedDependencyBlockVolumeBlocks(plan)
	if len(missed) == 0 {
		return
	}
	publishable := publishableDependencyBlockVolumeBlocks(missed)
	if len(publishable) == 0 {
		return
	}
	volumeIDs := make([]string, 0, len(publishable))
	for _, block := range publishable {
		volumeIDs = append(volumeIDs, blockVolumeID(dependencyVolumeStageName, block.CacheKey))
	}

	snapshotCfg := withSnapshotDriver(backendName, firecrackerCfg, firecrackerCfg.Snapshots.Driver)
	result, err := adapter.SnapshotCacheOutputVolumes(ctx, backend.SnapshotCacheOutputVolumesRequest{
		SandboxID:         sandboxID,
		SnapshotIDPrefix:  newSnapshotID(),
		VolumeIDs:         volumeIDs,
		FirecrackerConfig: snapshotCfg,
	})
	if err != nil {
		s.logDependencyStageWarning("publish dependency block-volume caches", sandboxID, err)
		return
	}

	snapshotsByVolumeID, err := dependencyBlockVolumeSnapshotsByVolumeID(volumeIDs, result)
	if err != nil {
		s.cleanupDependencyBlockVolumeSnapshots(adapter, backendName, firecrackerCfg, cacheOutputVolumeSnapshots(result))
		s.logDependencyStageWarning("publish dependency block-volume caches", sandboxID, err)
		return
	}

	now := s.clock().Now()
	records := make([]cachestore.Record, 0, len(publishable))
	snapshots := make([]backend.CacheOutputVolumeSnapshot, 0, len(publishable))
	for _, block := range publishable {
		volumeID := blockVolumeID(dependencyVolumeStageName, block.CacheKey)
		snapshot := snapshotsByVolumeID[volumeID]
		snapshots = append(snapshots, snapshot)
		record := dependencyBlockVolumeCacheRecordFromSnapshot(backendName, compiled, repository, changeset, block, snapshot, now)
		if reason := blockVolumeOutputRecordMissReason(block.Outputs, record.OutputRecords); reason != "" {
			s.cleanupDependencyBlockVolumeSnapshots(adapter, backendName, firecrackerCfg, snapshots)
			s.logDependencyStageWarning("publish dependency block-volume caches", sandboxID, fmt.Errorf("snapshot output records for block %q do not match declared outputs: %s", block.BlockName, reason))
			return
		}
		records = append(records, record)
	}

	for i, record := range records {
		if err := store.Create(ctx, record); err != nil {
			s.cleanupDependencyBlockVolumeSnapshots(adapter, backendName, firecrackerCfg, snapshots[i:])
			s.logDependencyStageWarning("persist dependency block-volume cache metadata", sandboxID, err)
			return
		}
		s.logDependencyBlockVolumePublished(record, sandboxID)
	}
}

func missedDependencyBlockVolumeBlocks(plan dependencyBlockVolumePlan) []dependencyBlockVolumeBlockPlan {
	missed := make([]dependencyBlockVolumeBlockPlan, 0, len(plan.Blocks))
	for _, block := range plan.Blocks {
		if !block.CacheHit {
			missed = append(missed, block)
		}
	}
	return missed
}

func publishableDependencyBlockVolumeBlocks(blocks []dependencyBlockVolumeBlockPlan) []dependencyBlockVolumeBlockPlan {
	publishable := make([]dependencyBlockVolumeBlockPlan, 0, len(blocks))
	for _, block := range blocks {
		if len(block.Outputs.Files) > 0 {
			continue
		}
		publishable = append(publishable, block)
	}
	return publishable
}

func dependencyBlockVolumeSnapshotsByVolumeID(volumeIDs []string, result *backend.SnapshotCacheOutputVolumesResult) (map[string]backend.CacheOutputVolumeSnapshot, error) {
	expected := make(map[string]struct{}, len(volumeIDs))
	for _, volumeID := range volumeIDs {
		volumeID = strings.TrimSpace(volumeID)
		if volumeID != "" {
			expected[volumeID] = struct{}{}
		}
	}
	snapshots := cacheOutputVolumeSnapshots(result)
	if len(snapshots) != len(expected) {
		return nil, fmt.Errorf("cache output snapshot returned %d volumes, expected %d", len(snapshots), len(expected))
	}
	byID := make(map[string]backend.CacheOutputVolumeSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		volumeID := strings.TrimSpace(snapshot.VolumeID)
		if _, ok := expected[volumeID]; !ok {
			return nil, fmt.Errorf("cache output snapshot returned unexpected volume id %q", snapshot.VolumeID)
		}
		if _, exists := byID[volumeID]; exists {
			return nil, fmt.Errorf("cache output snapshot returned duplicate volume id %q", snapshot.VolumeID)
		}
		if strings.TrimSpace(snapshot.SnapshotID) == "" {
			return nil, fmt.Errorf("cache output snapshot for volume %q is missing snapshot id", snapshot.VolumeID)
		}
		if strings.TrimSpace(snapshot.StorageDriver) == "" || strings.TrimSpace(snapshot.StorageRef) == "" {
			return nil, fmt.Errorf("cache output snapshot for volume %q is missing storage metadata", snapshot.VolumeID)
		}
		byID[volumeID] = snapshot
	}
	return byID, nil
}

func cacheOutputVolumeSnapshots(result *backend.SnapshotCacheOutputVolumesResult) []backend.CacheOutputVolumeSnapshot {
	if result == nil || len(result.Volumes) == 0 {
		return nil
	}
	return append([]backend.CacheOutputVolumeSnapshot(nil), result.Volumes...)
}

func dependencyBlockVolumeCacheRecordFromSnapshot(
	backendName string,
	compiled *policy.CompiledPolicy,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	block dependencyBlockVolumeBlockPlan,
	snapshot backend.CacheOutputVolumeSnapshot,
	now time.Time,
) cachestore.Record {
	record := cachestore.Record{
		CacheKey:                strings.TrimSpace(block.CacheKey),
		Stage:                   dependencyVolumeStageName,
		State:                   cacheStateReady,
		BackingSnapshotID:       strings.TrimSpace(snapshot.SnapshotID),
		Backend:                 strings.TrimSpace(backendName),
		Architecture:            runtime.GOARCH,
		PolicyHash:              compiled.Hash,
		Policy:                  compiled.ToProto(),
		Repository:              cloneRepositoryCheckout(normalizeRepositoryCheckoutForComparison(repository)).ToProto(),
		RepositoryHasChangeset:  changeset != nil,
		RepositoryChangesetID:   repositoryChangesetID(repository, changeset),
		StorageDriver:           strings.TrimSpace(snapshot.StorageDriver),
		StorageRef:              strings.TrimSpace(snapshot.StorageRef),
		StorageSizeBytes:        snapshot.StorageSizeBytes,
		ExclusiveSizeBytes:      snapshot.ExclusiveSizeBytes,
		DriverMetadata:          strings.TrimSpace(snapshot.DriverMetadata),
		InputManifestDigest:     strings.TrimSpace(block.InputManifestDigest),
		CommandDigest:           strings.TrimSpace(block.CommandDigest),
		EnvDigest:               strings.TrimSpace(block.EnvDigest),
		NormalizedOutputsDigest: strings.TrimSpace(block.NormalizedOutputsDigest),
		OutputRecords:           dependencyBlockVolumeOutputRecordsFromSnapshot(snapshot),
		CreatedAt:               now,
		LastUsedAt:              now,
		LastValidatedAt:         now.UTC(),
		ProducerVersion:         strings.TrimSpace(block.ProducerVersion),
	}
	return record
}

func dependencyBlockVolumeOutputRecordsFromSnapshot(snapshot backend.CacheOutputVolumeSnapshot) []cachestore.OutputRecord {
	if len(snapshot.Outputs) == 0 {
		return nil
	}
	records := make([]cachestore.OutputRecord, 0, len(snapshot.Outputs))
	for _, output := range snapshot.Outputs {
		records = append(records, cachestore.OutputRecord{
			Kind:          strings.TrimSpace(output.Kind),
			Path:          strings.TrimSpace(output.GuestPath),
			VolumeSubpath: strings.TrimSpace(output.VolumeSubpath),
			StorageDriver: strings.TrimSpace(snapshot.StorageDriver),
			StorageRef:    strings.TrimSpace(snapshot.StorageRef),
			SnapshotRef:   strings.TrimSpace(snapshot.SnapshotRef),
		})
	}
	return records
}

func (s *Service) cleanupDependencyBlockVolumeSnapshots(adapter backend.CacheOutputVolumeSnapshottingAdapter, backendName string, firecrackerCfg backend.FirecrackerConfig, snapshots []backend.CacheOutputVolumeSnapshot) {
	if len(snapshots) == 0 {
		return
	}
	deleteAdapter, ok := adapter.(backend.SnapshottingAdapter)
	if !ok {
		s.logDependencyStageWarning("rollback dependency block-volume cache snapshots", "", fmt.Errorf("backend %q cannot delete snapshots", backendName))
		return
	}
	for i := len(snapshots) - 1; i >= 0; i-- {
		snapshot := snapshots[i]
		storageRef := strings.TrimSpace(snapshot.StorageRef)
		if storageRef == "" {
			continue
		}
		deleteCfg := withSnapshotDriver(backendName, firecrackerCfg, snapshot.StorageDriver)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := deleteAdapter.DeleteSnapshot(cleanupCtx, backend.DeleteSnapshotRequest{
			SnapshotID:        strings.TrimSpace(snapshot.SnapshotID),
			StorageRef:        storageRef,
			FirecrackerConfig: deleteCfg,
		})
		cancel()
		if err != nil {
			s.logDependencyStageWarning("rollback dependency block-volume cache snapshot", "", err)
		}
	}
}

func (s *Service) logDependencyBlockVolumePublished(record cachestore.Record, sandboxID string) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Debug("dependency block-volume cache published",
		"stage", strings.TrimSpace(record.Stage),
		"cache_key", strings.TrimSpace(record.CacheKey),
		"producer_version", strings.TrimSpace(record.ProducerVersion),
		"storage_driver", strings.TrimSpace(record.StorageDriver),
		"storage_ref", strings.TrimSpace(record.StorageRef),
		"output_records", len(record.OutputRecords),
		"sandbox_id", strings.TrimSpace(sandboxID),
	)
}
