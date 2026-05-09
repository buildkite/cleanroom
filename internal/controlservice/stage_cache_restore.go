package controlservice

import (
	"context"
	"errors"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachestore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorybundle"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"go.opentelemetry.io/otel/attribute"
)

type createSandboxStageMaterializationConfig struct {
	BackendName  string
	Compiled     *policy.CompiledPolicy
	Repository   *repositorycheckout.Checkout
	CommitBundle *repositorybundle.Bundle
	Changeset    *repositorychangeset.Changeset
	Options      *cleanroomv1.SandboxOptions
	Reporter     CreateSandboxReporter
}

type createSandboxStageCacheRestoreConfig struct {
	Stage              string
	TraceName          string
	Phase              cleanroomv1.CreateSandboxPhase
	Message            string
	CacheOutputVolumes []backend.CacheOutputVolumeSpec
	LogTouchWarning    func(*Service, error)
	LogRestore         func(*Service, cachestore.Record, string)
	LogRestoreWarning  func(*Service, cachestore.Record, error)
}

type createSandboxStageCacheRestoreResult struct {
	Response       *cleanroomv1.CreateSandboxResponse
	ReplacedRecord *cachestore.Record
	Err            error
}

func (s *Service) restoreStageCacheForCreateSandbox(
	ctx context.Context,
	materialization createSandboxStageMaterializationConfig,
	record cachestore.Record,
	restore createSandboxStageCacheRestoreConfig,
) createSandboxStageCacheRestoreResult {
	emitCreateSandboxMessage(materialization.Reporter, restore.Phase, restore.Message)
	restoreReq := &cleanroomv1.CreateSandboxRequest{
		Backend: materialization.BackendName,
		Options: materialization.Options,
	}
	var restoreResp *cleanroomv1.CreateSandboxResponse
	restoreErr := s.traceCreateSandboxPhase(ctx, restore.TraceName, cachePhaseAttributes(
		restore.Stage,
		observability.CacheOperationRestore,
		materialization.Repository,
		attribute.String(observability.AttrBackend, materialization.BackendName),
	), func(ctx context.Context) error {
		var err error
		restoreResp, err = s.createSandboxFromCacheRecord(ctx, restoreReq, materialization.Compiled, record, restore.CacheOutputVolumes, materialization.Reporter)
		setCacheResultSpanAttribute(ctx, map[bool]string{true: observability.CacheResultFailed, false: observability.CacheResultRestored}[err != nil])
		return err
	})
	if restoreErr == nil {
		if cacheStore, err := s.cacheStoreOrErr(); err == nil {
			if err := cacheStore.Touch(ctx, record.Stage, record.CacheKey); err != nil && restore.LogTouchWarning != nil {
				restore.LogTouchWarning(s, err)
			}
		}
		s.retainRestoredSandboxRepositoryState(restoreResp, materialization.Repository, materialization.CommitBundle, materialization.Changeset)
		if restore.LogRestore != nil {
			restore.LogRestore(s, record, restoreResp.GetSandbox().GetSandboxId())
		}
		return createSandboxStageCacheRestoreResult{Response: restoreResp}
	}
	if errors.Is(restoreErr, errSandboxCreateAborted) {
		return createSandboxStageCacheRestoreResult{Err: restoreErr}
	}
	recordCopy := record
	if restore.LogRestoreWarning != nil {
		restore.LogRestoreWarning(s, record, restoreErr)
	}
	return createSandboxStageCacheRestoreResult{ReplacedRecord: &recordCopy}
}

func (s *Service) restoreWorkspaceStageCacheForCreateSandbox(
	ctx context.Context,
	materialization createSandboxStageMaterializationConfig,
	record cachestore.Record,
	cacheOutputVolumes []backend.CacheOutputVolumeSpec,
) createSandboxStageCacheRestoreResult {
	return s.restoreStageCacheForCreateSandbox(ctx, materialization, record, createSandboxStageCacheRestoreConfig{
		Stage:              observability.CacheStageWorkspace,
		TraceName:          "cleanroom.sandbox.restore_workspace_stage_cache",
		Phase:              cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_RESTORE_WORKSPACE_STAGE_CACHE,
		Message:            "restoring workspace stage cache",
		CacheOutputVolumes: cacheOutputVolumes,
		LogTouchWarning: func(s *Service, err error) {
			s.logWorkspaceStageWarning("touch workspace stage cache", "", err)
		},
		LogRestore: func(s *Service, record cachestore.Record, sandboxID string) {
			s.logWorkspaceStageRestore(record, sandboxID)
		},
		LogRestoreWarning: func(s *Service, record cachestore.Record, err error) {
			s.logWorkspaceStageRestoreWarning(record, err)
		},
	})
}

func (s *Service) restoreDependencyStageCacheForCreateSandbox(
	ctx context.Context,
	materialization createSandboxStageMaterializationConfig,
	record cachestore.Record,
	cacheOutputVolumes []backend.CacheOutputVolumeSpec,
) createSandboxStageCacheRestoreResult {
	return s.restoreStageCacheForCreateSandbox(ctx, materialization, record, createSandboxStageCacheRestoreConfig{
		Stage:              observability.CacheStageDependency,
		TraceName:          "cleanroom.sandbox.restore_dependency_stage_cache",
		Phase:              cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_RESTORE_DEPENDENCY_STAGE_CACHE,
		Message:            "restoring dependency stage cache",
		CacheOutputVolumes: cacheOutputVolumes,
		LogTouchWarning: func(s *Service, err error) {
			s.logDependencyStageWarning("touch dependency stage cache", "", err)
		},
		LogRestore: func(s *Service, record cachestore.Record, sandboxID string) {
			s.logDependencyStageRestore(record, sandboxID)
		},
		LogRestoreWarning: func(s *Service, record cachestore.Record, err error) {
			s.logDependencyStageRestoreWarning(record, err)
		},
	})
}

func (s *Service) restoreServicesStageCacheForCreateSandbox(
	ctx context.Context,
	materialization createSandboxStageMaterializationConfig,
	record cachestore.Record,
) createSandboxStageCacheRestoreResult {
	return s.restoreStageCacheForCreateSandbox(ctx, materialization, record, createSandboxStageCacheRestoreConfig{
		Stage:     observability.CacheStageServices,
		TraceName: "cleanroom.sandbox.restore_services_stage_cache",
		Phase:     cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_RESTORE_SERVICES_STAGE_CACHE,
		Message:   "restoring services stage cache",
		LogTouchWarning: func(s *Service, err error) {
			s.logServicesStageWarning("touch services stage cache", "", err)
		},
		LogRestore: func(s *Service, record cachestore.Record, sandboxID string) {
			s.logServicesStageRestore(record, sandboxID)
		},
		LogRestoreWarning: func(s *Service, record cachestore.Record, err error) {
			s.logServicesStageRestoreWarning(record, err)
		},
	})
}
