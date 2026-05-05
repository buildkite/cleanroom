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

type blockVolumePublishBlock struct {
	BlockName               string
	Outputs                 policy.StageBlockOutputs
	CacheKey                string
	CommandDigest           string
	EnvDigest               string
	InputManifestDigest     string
	NormalizedOutputsDigest string
	ProducerVersion         string
	CacheHit                bool
}

func blockVolumePublishBlockFromPlanBlock(block blockVolumeBlockPlan) blockVolumePublishBlock {
	return blockVolumePublishBlock{
		BlockName:               block.BlockName,
		Outputs:                 block.Outputs,
		CacheKey:                block.CacheKey,
		CommandDigest:           block.CommandDigest,
		EnvDigest:               block.EnvDigest,
		InputManifestDigest:     block.InputManifestDigest,
		NormalizedOutputsDigest: block.NormalizedOutputsDigest,
		ProducerVersion:         block.ProducerVersion,
		CacheHit:                block.CacheHit,
	}
}

type blockVolumePublishPhase struct {
	StageName               string
	PublishWarning          string
	PersistWarning          string
	RollbackWarning         string
	RollbackSnapshotWarning string
	PublishedMessage        string
	LogWarning              func(*Service, string, string, error)
}

func (phase blockVolumePublishPhase) warn(s *Service, message, sandboxID string, err error) {
	if phase.LogWarning == nil {
		return
	}
	phase.LogWarning(s, message, sandboxID, err)
}

func (s *Service) maybePublishBlockVolumeCaches(
	ctx context.Context,
	adapter backend.CacheOutputVolumeSnapshottingAdapter,
	sandboxID, backendName string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	phase blockVolumePublishPhase,
	blocks []blockVolumePublishBlock,
) {
	if adapter == nil || compiled == nil || repository == nil || strings.TrimSpace(sandboxID) == "" || strings.TrimSpace(phase.StageName) == "" || len(blocks) == 0 {
		return
	}
	store, err := s.cacheStoreOrErr()
	if err != nil {
		phase.warn(s, phase.PublishWarning, sandboxID, err)
		return
	}

	missed := missedBlockVolumePublishBlocks(blocks)
	if len(missed) == 0 {
		return
	}
	volumeIDs := make([]string, 0, len(missed))
	for _, block := range missed {
		volumeIDs = append(volumeIDs, blockVolumeID(phase.StageName, block.CacheKey))
	}

	snapshotCfg := withSnapshotDriver(backendName, firecrackerCfg, firecrackerCfg.Snapshots.Driver)
	result, err := adapter.SnapshotCacheOutputVolumes(ctx, backend.SnapshotCacheOutputVolumesRequest{
		SandboxID:         sandboxID,
		SnapshotIDPrefix:  newSnapshotID(),
		VolumeIDs:         volumeIDs,
		FirecrackerConfig: snapshotCfg,
	})
	if err != nil {
		phase.warn(s, phase.PublishWarning, sandboxID, err)
		return
	}

	snapshotsByVolumeID, err := blockVolumeSnapshotsByVolumeID(volumeIDs, result)
	if err != nil {
		s.cleanupBlockVolumeSnapshots(adapter, backendName, firecrackerCfg, phase, cacheOutputVolumeSnapshots(result))
		phase.warn(s, phase.PublishWarning, sandboxID, err)
		return
	}

	now := s.clock().Now()
	records := make([]cachestore.Record, 0, len(missed))
	snapshots := make([]backend.CacheOutputVolumeSnapshot, 0, len(missed))
	for _, block := range missed {
		volumeID := blockVolumeID(phase.StageName, block.CacheKey)
		snapshots = append(snapshots, snapshotsByVolumeID[volumeID])
	}
	for i, block := range missed {
		snapshot := snapshots[i]
		record := blockVolumeCacheRecordFromSnapshot(backendName, compiled, repository, changeset, phase.StageName, block, snapshot, now)
		if reason := blockVolumeOutputRecordMissReason(block.Outputs, record.OutputRecords); reason != "" {
			s.cleanupBlockVolumeSnapshots(adapter, backendName, firecrackerCfg, phase, snapshots)
			phase.warn(s, phase.PublishWarning, sandboxID, fmt.Errorf("snapshot output records for block %q do not match declared outputs: %s", block.BlockName, reason))
			return
		}
		records = append(records, record)
	}

	for i, record := range records {
		if err := store.Create(ctx, record); err != nil {
			s.cleanupBlockVolumeSnapshots(adapter, backendName, firecrackerCfg, phase, snapshots[i:])
			phase.warn(s, phase.PersistWarning, sandboxID, err)
			return
		}
		s.logBlockVolumePublished(phase, record, sandboxID)
	}
}

func missedBlockVolumePublishBlocks(blocks []blockVolumePublishBlock) []blockVolumePublishBlock {
	missed := make([]blockVolumePublishBlock, 0, len(blocks))
	for _, block := range blocks {
		if !block.CacheHit {
			missed = append(missed, block)
		}
	}
	return missed
}

func blockVolumeSnapshotsByVolumeID(volumeIDs []string, result *backend.SnapshotCacheOutputVolumesResult) (map[string]backend.CacheOutputVolumeSnapshot, error) {
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

func blockVolumeCacheRecordFromSnapshot(
	backendName string,
	compiled *policy.CompiledPolicy,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	stageName string,
	block blockVolumePublishBlock,
	snapshot backend.CacheOutputVolumeSnapshot,
	now time.Time,
) cachestore.Record {
	record := cachestore.Record{
		CacheKey:                strings.TrimSpace(block.CacheKey),
		Stage:                   strings.TrimSpace(stageName),
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
		OutputRecords:           blockVolumeOutputRecordsFromSnapshot(snapshot),
		CreatedAt:               now,
		LastUsedAt:              now,
		LastValidatedAt:         now.UTC(),
		ProducerVersion:         strings.TrimSpace(block.ProducerVersion),
	}
	return record
}

func blockVolumeOutputRecordsFromSnapshot(snapshot backend.CacheOutputVolumeSnapshot) []cachestore.OutputRecord {
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

func (s *Service) cleanupBlockVolumeSnapshots(
	adapter backend.CacheOutputVolumeSnapshottingAdapter,
	backendName string,
	firecrackerCfg backend.FirecrackerConfig,
	phase blockVolumePublishPhase,
	snapshots []backend.CacheOutputVolumeSnapshot,
) {
	if len(snapshots) == 0 {
		return
	}
	deleteAdapter, ok := adapter.(backend.SnapshottingAdapter)
	if !ok {
		phase.warn(s, phase.RollbackWarning, "", fmt.Errorf("backend %q cannot delete snapshots", backendName))
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
			phase.warn(s, phase.RollbackSnapshotWarning, "", err)
		}
	}
}

func (s *Service) logBlockVolumePublished(phase blockVolumePublishPhase, record cachestore.Record, sandboxID string) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Debug(phase.PublishedMessage,
		"stage", strings.TrimSpace(record.Stage),
		"cache_key", strings.TrimSpace(record.CacheKey),
		"producer_version", strings.TrimSpace(record.ProducerVersion),
		"storage_driver", strings.TrimSpace(record.StorageDriver),
		"storage_ref", strings.TrimSpace(record.StorageRef),
		"output_records", len(record.OutputRecords),
		"sandbox_id", strings.TrimSpace(sandboxID),
	)
}
