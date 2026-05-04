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
	if adapter == nil || compiled == nil || repository == nil || strings.TrimSpace(sandboxID) == "" || len(plan.Blocks) == 0 {
		return
	}
	store, err := s.cacheStoreOrErr()
	if err != nil {
		s.logServicesStageWarning("publish service block-volume caches", sandboxID, err)
		return
	}

	missed := missedServiceBlockVolumeBlocks(plan)
	if len(missed) == 0 {
		return
	}
	publishable := publishableServiceBlockVolumeBlocks(missed)
	if len(publishable) == 0 {
		return
	}
	volumeIDs := make([]string, 0, len(publishable))
	for _, block := range publishable {
		volumeIDs = append(volumeIDs, blockVolumeID(serviceVolumeStageName, block.CacheKey))
	}

	snapshotCfg := withSnapshotDriver(backendName, firecrackerCfg, firecrackerCfg.Snapshots.Driver)
	result, err := adapter.SnapshotCacheOutputVolumes(ctx, backend.SnapshotCacheOutputVolumesRequest{
		SandboxID:         sandboxID,
		SnapshotIDPrefix:  newSnapshotID(),
		VolumeIDs:         volumeIDs,
		FirecrackerConfig: snapshotCfg,
	})
	if err != nil {
		s.logServicesStageWarning("publish service block-volume caches", sandboxID, err)
		return
	}

	snapshotsByVolumeID, err := serviceBlockVolumeSnapshotsByVolumeID(volumeIDs, result)
	if err != nil {
		s.cleanupServiceBlockVolumeSnapshots(adapter, backendName, firecrackerCfg, cacheOutputVolumeSnapshots(result))
		s.logServicesStageWarning("publish service block-volume caches", sandboxID, err)
		return
	}

	now := s.clock().Now()
	records := make([]cachestore.Record, 0, len(publishable))
	snapshots := make([]backend.CacheOutputVolumeSnapshot, 0, len(publishable))
	for _, block := range publishable {
		volumeID := blockVolumeID(serviceVolumeStageName, block.CacheKey)
		snapshot := snapshotsByVolumeID[volumeID]
		snapshots = append(snapshots, snapshot)
		record := serviceBlockVolumeCacheRecordFromSnapshot(backendName, compiled, repository, changeset, block, snapshot, now)
		if reason := blockVolumeOutputRecordMissReason(block.Outputs, record.OutputRecords); reason != "" {
			s.cleanupServiceBlockVolumeSnapshots(adapter, backendName, firecrackerCfg, snapshots)
			s.logServicesStageWarning("publish service block-volume caches", sandboxID, fmt.Errorf("snapshot output records for block %q do not match declared outputs: %s", block.BlockName, reason))
			return
		}
		records = append(records, record)
	}

	for i, record := range records {
		if err := store.Create(ctx, record); err != nil {
			s.cleanupServiceBlockVolumeSnapshots(adapter, backendName, firecrackerCfg, snapshots[i:])
			s.logServicesStageWarning("persist service block-volume cache metadata", sandboxID, err)
			return
		}
		s.logServiceBlockVolumePublished(record, sandboxID)
	}
}

func missedServiceBlockVolumeBlocks(plan serviceBlockVolumePlan) []serviceBlockVolumeBlockPlan {
	missed := make([]serviceBlockVolumeBlockPlan, 0, len(plan.Blocks))
	for _, block := range plan.Blocks {
		if !block.CacheHit {
			missed = append(missed, block)
		}
	}
	return missed
}

func publishableServiceBlockVolumeBlocks(blocks []serviceBlockVolumeBlockPlan) []serviceBlockVolumeBlockPlan {
	publishable := make([]serviceBlockVolumeBlockPlan, 0, len(blocks))
	for _, block := range blocks {
		if len(block.Outputs.Files) > 0 {
			continue
		}
		publishable = append(publishable, block)
	}
	return publishable
}

func serviceBlockVolumeSnapshotsByVolumeID(volumeIDs []string, result *backend.SnapshotCacheOutputVolumesResult) (map[string]backend.CacheOutputVolumeSnapshot, error) {
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

func serviceBlockVolumeCacheRecordFromSnapshot(
	backendName string,
	compiled *policy.CompiledPolicy,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	block serviceBlockVolumeBlockPlan,
	snapshot backend.CacheOutputVolumeSnapshot,
	now time.Time,
) cachestore.Record {
	record := cachestore.Record{
		CacheKey:                strings.TrimSpace(block.CacheKey),
		Stage:                   serviceVolumeStageName,
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
		OutputRecords:           serviceBlockVolumeOutputRecordsFromSnapshot(snapshot),
		CreatedAt:               now,
		LastUsedAt:              now,
		LastValidatedAt:         now.UTC(),
		ProducerVersion:         strings.TrimSpace(block.ProducerVersion),
	}
	return record
}

func serviceBlockVolumeOutputRecordsFromSnapshot(snapshot backend.CacheOutputVolumeSnapshot) []cachestore.OutputRecord {
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

func (s *Service) cleanupServiceBlockVolumeSnapshots(adapter backend.CacheOutputVolumeSnapshottingAdapter, backendName string, firecrackerCfg backend.FirecrackerConfig, snapshots []backend.CacheOutputVolumeSnapshot) {
	if len(snapshots) == 0 {
		return
	}
	deleteAdapter, ok := adapter.(backend.SnapshottingAdapter)
	if !ok {
		s.logServicesStageWarning("rollback service block-volume cache snapshots", "", fmt.Errorf("backend %q cannot delete snapshots", backendName))
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
			s.logServicesStageWarning("rollback service block-volume cache snapshot", "", err)
		}
	}
}

func (s *Service) logServiceBlockVolumePublished(record cachestore.Record, sandboxID string) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Debug("service block-volume cache published",
		"stage", strings.TrimSpace(record.Stage),
		"cache_key", strings.TrimSpace(record.CacheKey),
		"producer_version", strings.TrimSpace(record.ProducerVersion),
		"storage_driver", strings.TrimSpace(record.StorageDriver),
		"storage_ref", strings.TrimSpace(record.StorageRef),
		"output_records", len(record.OutputRecords),
		"sandbox_id", strings.TrimSpace(sandboxID),
	)
}
