package controlservice

import (
	"context"
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachekey"
	"github.com/buildkite/cleanroom/internal/cachestore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorybundle"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

const (
	servicesStageName            = "services"
	servicesStageProducerVersion = "cleanroom/services-stage-v1"
)

type servicesStagePlan struct {
	CacheKey              string
	ParentStageCacheKey   string
	BootstrapCommand      []string
	BootstrapRecipeDigest string
	KeyFiles              []string
	KeyFilesDigest        string
}

func servicesStagePlanForRepository(compiled *policy.CompiledPolicy, repository *repositorycheckout.Checkout) (servicesStagePlan, bool) {
	if compiled == nil || repository == nil || !compiled.Services.BootstrapEnabled() {
		return servicesStagePlan{}, false
	}

	bootstrapCommand := repositorycheckout.WrapCommandInWorkdir(compiled.Services.Command, repository)
	bootstrapRecipeDigest := repositorycheckout.WorkdirRecipeDigest(compiled.Services.Command, repository)
	if len(bootstrapCommand) == 0 || strings.TrimSpace(bootstrapRecipeDigest) == "" {
		return servicesStagePlan{}, false
	}

	return servicesStagePlan{
		BootstrapCommand:      bootstrapCommand,
		BootstrapRecipeDigest: bootstrapRecipeDigest,
		KeyFiles:              append([]string(nil), compiled.Services.KeyFiles...),
	}, true
}

func (s *Service) finalizeServicesStagePlan(
	ctx context.Context,
	compiled *policy.CompiledPolicy,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	commitBundle *repositorybundle.Bundle,
	parentStageKey string,
	plan servicesStagePlan,
) (servicesStagePlan, bool, error) {
	if compiled == nil || repository == nil || strings.TrimSpace(parentStageKey) == "" {
		return plan, false, nil
	}

	keyFilesDigest, err := s.stageKeyFilesDigest(ctx, repository, changeset, commitBundle, plan.KeyFiles, servicesStageName)
	if err != nil {
		return plan, false, err
	}

	cacheKey := cachekey.ServicesStageKey(cachekey.ServicesStageInputs{
		ParentStageKey:        strings.TrimSpace(parentStageKey),
		CompiledPolicyHash:    strings.TrimSpace(compiled.Hash),
		KeyFilesDigest:        strings.TrimSpace(keyFilesDigest),
		BootstrapRecipeDigest: strings.TrimSpace(plan.BootstrapRecipeDigest),
	})
	if strings.TrimSpace(cacheKey) == "" {
		return plan, false, nil
	}

	plan.CacheKey = cacheKey
	plan.ParentStageCacheKey = strings.TrimSpace(parentStageKey)
	plan.KeyFilesDigest = strings.TrimSpace(keyFilesDigest)
	return plan, true, nil
}

func (s *Service) lookupServicesStageCache(ctx context.Context, backendName string, compiled *policy.CompiledPolicy, repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset, plan servicesStagePlan) (cachestore.Record, bool, string, error) {
	if compiled == nil || repository == nil || strings.TrimSpace(plan.CacheKey) == "" {
		return cachestore.Record{}, false, "", nil
	}
	store, err := s.cacheStoreOrErr()
	if err != nil {
		return cachestore.Record{}, false, "", nil
	}

	record, ok, err := store.GetReady(ctx, servicesStageName, plan.CacheKey)
	if err != nil {
		return cachestore.Record{}, false, "", err
	}
	if !ok {
		return cachestore.Record{}, false, observability.CacheLookupReasonRecordNotFound, nil
	}
	if strings.TrimSpace(record.Backend) != strings.TrimSpace(backendName) {
		return cachestore.Record{}, false, observability.CacheLookupReasonBackendMismatch, nil
	}
	if strings.TrimSpace(record.PolicyHash) != strings.TrimSpace(compiled.Hash) {
		return cachestore.Record{}, false, observability.CacheLookupReasonPolicyHashMismatch, nil
	}
	if strings.TrimSpace(record.ParentCacheKey) != strings.TrimSpace(plan.ParentStageCacheKey) {
		return cachestore.Record{}, false, observability.CacheLookupReasonParentStageChanged, nil
	}
	if !repositoryCheckoutsEqual(repositorycheckout.FromProto(record.Repository), repository) {
		return cachestore.Record{}, false, observability.CacheLookupReasonRepositoryChanged, nil
	}
	if !cacheRecordChangesetIDMatches(record.RepositoryChangesetID, repository, changeset) {
		return cachestore.Record{}, false, observability.CacheLookupReasonRepositoryChanged, nil
	}
	return record, true, "", nil
}

func (s *Service) maybePublishServicesStageCache(
	ctx context.Context,
	adapter backend.SnapshottingAdapter,
	sandboxID, backendName string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	plan servicesStagePlan,
	replacedRecord *cachestore.Record,
) {
	if adapter == nil || compiled == nil || repository == nil || strings.TrimSpace(plan.CacheKey) == "" {
		return
	}
	if !snapshotOperationsEnabledForBackend(backendName, s.Config) {
		return
	}

	store, err := s.cacheStoreOrErr()
	if err != nil {
		return
	}

	if record, ok, _, err := s.lookupServicesStageCache(ctx, backendName, compiled, repository, changeset, plan); err == nil && ok {
		if replacedRecord == nil || strings.TrimSpace(record.CacheKey) != strings.TrimSpace(replacedRecord.CacheKey) {
			s.logServicesStageAlreadyPublished(record)
			return
		}
	} else if err != nil {
		s.logServicesStageWarning("lookup services stage cache", sandboxID, err)
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
		s.logServicesStageWarning("publish services stage cache", sandboxID, err)
		return
	}

	record := cachestore.Record{
		CacheKey:               plan.CacheKey,
		Stage:                  servicesStageName,
		State:                  cacheStateReady,
		BackingSnapshotID:      strings.TrimSpace(snapshotID),
		Backend:                backendName,
		PolicyHash:             compiled.Hash,
		Policy:                 compiled.ToProto(),
		Repository:             cloneRepositoryCheckout(normalizeRepositoryCheckoutForComparison(repository)).ToProto(),
		RepositoryHasChangeset: changeset != nil,
		RepositoryChangesetID:  repositoryChangesetID(repository, changeset),
		ParentCacheKey:         plan.ParentStageCacheKey,
		StorageDriver:          snapshotCfg.Snapshots.Driver,
		StorageRef:             strings.TrimSpace(result.StorageRef),
		CreatedAt:              s.clock().Now(),
		LastUsedAt:             s.clock().Now(),
		ProducerVersion:        servicesStageProducerVersion,
	}

	persist := store.Create
	if replacedRecord != nil && strings.TrimSpace(replacedRecord.CacheKey) == plan.CacheKey {
		persist = store.Upsert
	}

	if err := persist(ctx, record); err != nil {
		deleteErr := adapter.DeleteSnapshot(ctx, backend.DeleteSnapshotRequest{
			SnapshotID:        snapshotID,
			StorageRef:        record.StorageRef,
			FirecrackerConfig: snapshotCfg,
		})
		if deleteErr != nil {
			s.logServicesStageWarning("rollback services stage cache after metadata failure", sandboxID, fmt.Errorf("%w (rollback failed: %v)", err, deleteErr))
			return
		}
		s.logServicesStageWarning("persist services stage cache metadata", sandboxID, err)
		return
	}

	s.logServicesStagePublished(record, sandboxID, replacedRecord != nil && strings.TrimSpace(replacedRecord.CacheKey) == plan.CacheKey)

	if replacedRecord != nil && strings.TrimSpace(replacedRecord.CacheKey) == plan.CacheKey {
		if err := s.deleteWorkspaceStageCacheSnapshot(ctx, adapter, backendName, firecrackerCfg, *replacedRecord); err != nil {
			s.logServicesStageWarning("delete replaced services stage cache snapshot", sandboxID, err)
		}
	}
}

func (s *Service) bootstrapServicesStageInPersistentSandbox(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	plan servicesStagePlan,
	reporter CreateSandboxReporter,
) error {
	if adapter == nil || compiled == nil || strings.TrimSpace(sandboxID) == "" || len(plan.BootstrapCommand) == 0 {
		return nil
	}
	if s.Logger != nil {
		s.Logger.Debug("starting services stage bootstrap",
			"sandbox_id", sandboxID,
			"cache_key", strings.TrimSpace(plan.CacheKey),
		)
	}

	bootstrapExecutionID, result, stdout, stderr, err := s.runPersistentBootstrapCommand(
		ctx,
		adapter,
		sandboxID,
		compiled,
		firecrackerCfg,
		cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_SERVICES,
		policy.NetworkStageServices,
		plan.BootstrapCommand,
		nil,
		reporter,
	)
	if s.Logger != nil {
		s.Logger.Debug("services stage bootstrap execution finished",
			"sandbox_id", sandboxID,
			"execution_id", bootstrapExecutionID,
			"exit_code", func() int {
				if result == nil {
					return -1
				}
				return result.ExitCode
			}(),
			"error", err,
		)
	}
	return persistentBootstrapCommandError(result, stdout, stderr, err, "services stage bootstrap returned no result", "services stage bootstrap failed with exit code %d")
}

func (s *Service) logServicesStageCacheHit(record cachestore.Record) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Debug("services stage cache hit",
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backing_snapshot_id", strings.TrimSpace(record.BackingSnapshotID),
		"backend", strings.TrimSpace(record.Backend),
	)
}

func (s *Service) logServicesStageCacheMiss(backendName, cacheKey string) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Debug("services stage cache miss",
		"cache_key", strings.TrimSpace(cacheKey),
		"backend", strings.TrimSpace(backendName),
	)
}

func (s *Service) logServicesStageAlreadyPublished(record cachestore.Record) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Debug("services stage cache already published",
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backing_snapshot_id", strings.TrimSpace(record.BackingSnapshotID),
		"backend", strings.TrimSpace(record.Backend),
	)
}

func (s *Service) logServicesStagePublished(record cachestore.Record, sandboxID string, replaced bool) {
	if s == nil || s.Logger == nil {
		return
	}
	message := "services stage cache published"
	if replaced {
		message = "services stage cache replaced"
	}
	s.Logger.Info(message,
		"sandbox_id", strings.TrimSpace(sandboxID),
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backing_snapshot_id", strings.TrimSpace(record.BackingSnapshotID),
		"backend", strings.TrimSpace(record.Backend),
	)
}

func (s *Service) logServicesStageRestore(record cachestore.Record, sandboxID string) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Info("services stage cache restored",
		"sandbox_id", strings.TrimSpace(sandboxID),
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backing_snapshot_id", strings.TrimSpace(record.BackingSnapshotID),
		"backend", strings.TrimSpace(record.Backend),
	)
}

func (s *Service) logServicesStageRestoreWarning(record cachestore.Record, err error) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Warn("restore services stage cache",
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backing_snapshot_id", strings.TrimSpace(record.BackingSnapshotID),
		"backend", strings.TrimSpace(record.Backend),
		"error", err,
	)
}

func (s *Service) logServicesStageWarning(operation, sandboxID string, err error) {
	if s == nil || s.Logger == nil || err == nil {
		return
	}
	s.Logger.Warn(operation,
		"sandbox_id", strings.TrimSpace(sandboxID),
		"error", err,
	)
}
