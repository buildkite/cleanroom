package controlservice

import (
	"context"
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachekey"
	"github.com/buildkite/cleanroom/internal/cachestore"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

const (
	workspaceStageName            = "workspace"
	cacheStateReady               = "ready"
	workspaceStageProducerVersion = "cleanroom/workspace-stage-v1"
)

func workspaceStageCacheKey(backendName, runtimeBaseKey, compiledPolicyHash string, repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset) string {
	normalizedRepository := normalizeRepositoryCheckoutForComparison(repository)
	if normalizedRepository == nil || strings.TrimSpace(backendName) == "" || strings.TrimSpace(runtimeBaseKey) == "" || strings.TrimSpace(compiledPolicyHash) == "" {
		return ""
	}
	return cachekey.WorkspaceStageKey(cachekey.WorkspaceStageInputs{
		Backend:                     strings.TrimSpace(backendName),
		RuntimeKey:                  strings.TrimSpace(runtimeBaseKey),
		CompiledPolicyHash:          strings.TrimSpace(compiledPolicyHash),
		CanonicalRemoteURL:          strings.TrimSpace(normalizedRepository.RemoteURL),
		CommitSHA:                   strings.TrimSpace(normalizedRepository.CommitSHA),
		SubmoduleMode:               workspaceStageSubmoduleMode(normalizedRepository),
		SubmoduleResolutionDigest:   "",
		ChangesetDigest:             strings.TrimSpace(changesetDigest(changeset)),
		CheckoutMode:                workspaceStageCheckoutMode(normalizedRepository),
		DestinationDir:              strings.TrimSpace(normalizedRepository.DestinationDir),
		MaterializationRecipeDigest: workspaceStageMaterializationRecipeDigest(normalizedRepository),
	})
}

func changesetDigest(changeset *repositorychangeset.Changeset) string {
	if changeset == nil {
		return ""
	}
	return strings.TrimSpace(changeset.Digest)
}

func workspaceStageCheckoutMode(repository *repositorycheckout.Checkout) string {
	if repository == nil {
		return ""
	}
	if branch := strings.TrimSpace(repository.Branch); branch != "" {
		return "branch:" + branch
	}
	return "detached"
}

func workspaceStageSubmoduleMode(repository *repositorycheckout.Checkout) string {
	if repository == nil {
		return ""
	}
	if repository.Submodules {
		return "enabled"
	}
	return "disabled"
}

func workspaceStageMaterializationRecipeDigest(repository *repositorycheckout.Checkout) string {
	return repositorycheckout.BootstrapRecipeDigest(repository)
}

func (s *Service) lookupWorkspaceStageCache(ctx context.Context, backendName string, compiled *policy.CompiledPolicy, runtimeBaseKey string, repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset) (cachestore.Record, bool, error) {
	if compiled == nil || repository == nil || strings.TrimSpace(runtimeBaseKey) == "" {
		return cachestore.Record{}, false, nil
	}
	store, err := s.cacheStoreOrErr()
	if err != nil {
		return cachestore.Record{}, false, nil
	}
	cacheKey := workspaceStageCacheKey(backendName, runtimeBaseKey, compiled.Hash, repository, changeset)
	if cacheKey == "" {
		return cachestore.Record{}, false, nil
	}

	record, ok, err := store.GetReady(ctx, workspaceStageName, cacheKey)
	if err != nil {
		return cachestore.Record{}, false, err
	}
	if !ok {
		return cachestore.Record{}, false, nil
	}
	if strings.TrimSpace(record.Backend) != strings.TrimSpace(backendName) {
		return cachestore.Record{}, false, nil
	}
	if !repositoryCheckoutsEqual(repositorycheckout.FromProto(record.Repository), repository) {
		return cachestore.Record{}, false, nil
	}
	if strings.TrimSpace(record.PolicyHash) != strings.TrimSpace(compiled.Hash) {
		return cachestore.Record{}, false, nil
	}
	return record, true, nil
}

func (s *Service) maybePublishWorkspaceStageCache(
	ctx context.Context,
	adapter backend.SnapshottingAdapter,
	sandboxID, backendName string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	runtimeBaseKey string,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	replacedRecord *cachestore.Record,
) {
	if adapter == nil || compiled == nil || repository == nil || strings.TrimSpace(runtimeBaseKey) == "" {
		return
	}
	if !snapshotOperationsEnabledForBackend(backendName, s.Config) {
		return
	}

	store, err := s.cacheStoreOrErr()
	if err != nil {
		return
	}

	cacheKey := workspaceStageCacheKey(backendName, runtimeBaseKey, compiled.Hash, repository, changeset)
	if cacheKey == "" {
		return
	}

	if record, ok, err := s.lookupWorkspaceStageCache(ctx, backendName, compiled, runtimeBaseKey, repository, changeset); err == nil && ok {
		if replacedRecord == nil || strings.TrimSpace(record.CacheKey) != strings.TrimSpace(replacedRecord.CacheKey) {
			s.logWorkspaceStageAlreadyPublished(record)
			return
		}
	} else if err != nil {
		s.logWorkspaceStageWarning("lookup workspace stage cache", sandboxID, err)
		return
	}

	snapshotID := newSnapshotID()
	snapshotCfg := withSnapshotDriver(backendName, firecrackerCfg, firecrackerCfg.Snapshots.Driver)
	result, err := adapter.CreateSnapshot(ctx, backend.SnapshotRequest{
		SandboxID:         sandboxID,
		SnapshotID:        snapshotID,
		FirecrackerConfig: snapshotCfg,
	})
	if err != nil {
		s.logWorkspaceStageWarning("publish workspace stage cache", sandboxID, err)
		return
	}

	record := cachestore.Record{
		CacheKey:          cacheKey,
		Stage:             workspaceStageName,
		State:             cacheStateReady,
		BackingSnapshotID: strings.TrimSpace(snapshotID),
		Backend:           backendName,
		PolicyHash:        compiled.Hash,
		Policy:            compiled.ToProto(),
		Repository:        cloneRepositoryCheckout(normalizeRepositoryCheckoutForComparison(repository)).ToProto(),
		ParentCacheKey:    strings.TrimSpace(runtimeBaseKey),
		StorageDriver:     snapshotCfg.Snapshots.Driver,
		StorageRef:        strings.TrimSpace(result.StorageRef),
		CreatedAt:         s.clock().Now(),
		LastUsedAt:        s.clock().Now(),
		ProducerVersion:   workspaceStageProducerVersion,
	}

	persist := store.Create
	if replacedRecord != nil && strings.TrimSpace(replacedRecord.CacheKey) == cacheKey {
		persist = store.Upsert
	}

	if err := persist(ctx, record); err != nil {
		deleteErr := adapter.DeleteSnapshot(ctx, backend.DeleteSnapshotRequest{
			SnapshotID:        snapshotID,
			StorageRef:        record.StorageRef,
			FirecrackerConfig: snapshotCfg,
		})
		if deleteErr != nil {
			s.logWorkspaceStageWarning("rollback workspace stage cache after metadata failure", sandboxID, fmt.Errorf("%w (rollback failed: %v)", err, deleteErr))
			return
		}
		s.logWorkspaceStageWarning("persist workspace stage cache metadata", sandboxID, err)
		return
	}

	s.logWorkspaceStagePublished(record, sandboxID, replacedRecord != nil && strings.TrimSpace(replacedRecord.CacheKey) == cacheKey)

	if replacedRecord != nil && strings.TrimSpace(replacedRecord.CacheKey) == cacheKey {
		if err := s.deleteWorkspaceStageCacheSnapshot(ctx, adapter, backendName, firecrackerCfg, *replacedRecord); err != nil {
			s.logWorkspaceStageWarning("delete replaced workspace stage cache snapshot", sandboxID, err)
		}
	}
}

func (s *Service) deleteWorkspaceStageCacheSnapshot(ctx context.Context, adapter backend.SnapshottingAdapter, backendName string, firecrackerCfg backend.FirecrackerConfig, record cachestore.Record) error {
	if adapter == nil {
		return nil
	}
	storageRef := strings.TrimSpace(record.StorageRef)
	if storageRef == "" {
		return nil
	}
	snapshotID := strings.TrimSpace(record.BackingSnapshotID)
	if snapshotID == "" {
		snapshotID = strings.TrimSpace(record.CacheKey)
	}
	if snapshotID != "" {
		if err := s.beginSnapshotDelete(snapshotID); err != nil {
			return err
		}
		defer s.finishSnapshotDelete(snapshotID)
	}
	deleteCfg := withSnapshotDriver(backendName, firecrackerCfg, record.StorageDriver)
	return adapter.DeleteSnapshot(ctx, backend.DeleteSnapshotRequest{
		SnapshotID:        snapshotID,
		StorageRef:        storageRef,
		FirecrackerConfig: deleteCfg,
	})
}

func (s *Service) workspaceStageRuntimeBaseKey(ctx context.Context, adapter backend.Adapter, compiled *policy.CompiledPolicy, firecrackerCfg backend.FirecrackerConfig) (string, bool, error) {
	if adapter == nil || compiled == nil {
		return "", false, nil
	}
	provider, ok := adapter.(backend.RuntimeBaseKeyProvider)
	if !ok {
		return "", false, nil
	}
	key, err := provider.RuntimeBaseKey(ctx, compiled, firecrackerCfg)
	if err != nil {
		return "", false, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", false, nil
	}
	return key, true, nil
}

func (s *Service) logWorkspaceStageCacheHit(record cachestore.Record) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Debug("workspace stage cache hit",
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backing_snapshot_id", strings.TrimSpace(record.BackingSnapshotID),
		"backend", strings.TrimSpace(record.Backend),
	)
}

func (s *Service) logWorkspaceStageCacheMiss(backendName, cacheKey string) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Debug("workspace stage cache miss",
		"cache_key", strings.TrimSpace(cacheKey),
		"backend", strings.TrimSpace(backendName),
	)
}

func (s *Service) logWorkspaceStageAlreadyPublished(record cachestore.Record) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Debug("workspace stage cache already published",
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backing_snapshot_id", strings.TrimSpace(record.BackingSnapshotID),
		"backend", strings.TrimSpace(record.Backend),
	)
}

func (s *Service) logWorkspaceStagePublished(record cachestore.Record, sandboxID string, replaced bool) {
	if s == nil || s.Logger == nil {
		return
	}
	message := "workspace stage cache published"
	if replaced {
		message = "workspace stage cache replaced"
	}
	s.Logger.Info(message,
		"sandbox_id", strings.TrimSpace(sandboxID),
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backing_snapshot_id", strings.TrimSpace(record.BackingSnapshotID),
		"backend", strings.TrimSpace(record.Backend),
	)
}

func (s *Service) logWorkspaceStageRestore(record cachestore.Record, sandboxID string) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Info("workspace stage cache restored",
		"sandbox_id", strings.TrimSpace(sandboxID),
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backing_snapshot_id", strings.TrimSpace(record.BackingSnapshotID),
		"backend", strings.TrimSpace(record.Backend),
	)
}

func (s *Service) logWorkspaceStageWarning(message, sandboxID string, err error) {
	if s == nil || s.Logger == nil || err == nil {
		return
	}
	s.Logger.Warn(message, "sandbox_id", sandboxID, "error", err)
}

func (s *Service) logWorkspaceStageRestoreWarning(record cachestore.Record, err error) {
	if s == nil || s.Logger == nil || err == nil {
		return
	}
	s.Logger.Warn("restore workspace stage cache",
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backing_snapshot_id", strings.TrimSpace(record.BackingSnapshotID),
		"backend", strings.TrimSpace(record.Backend),
		"storage_ref", strings.TrimSpace(record.StorageRef),
		"error", err,
	)
}
