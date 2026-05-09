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

type servicesStageResolveRequest struct {
	stageCacheResolveContext
	plan            servicesStagePlan
	afterDependency bool
}

type stageCacheResolveContext struct {
	backendName     string
	snapshotAdapter backend.SnapshottingAdapter
	compiled        *policy.CompiledPolicy
	firecrackerCfg  backend.FirecrackerConfig
	repository      *repositorycheckout.Checkout
	changeset       *repositorychangeset.Changeset
	commitBundle    *repositorybundle.Bundle
	options         *cleanroomv1.SandboxOptions
	reporter        CreateSandboxReporter
}

type servicesStageResolveResult struct {
	restored *cleanroomv1.CreateSandboxResponse
	replaced *cachestore.Record
}

type dependencyStageResolveRequest struct {
	stageCacheResolveContext
	plan                dependencyStagePlan
	servicesPlan        servicesStagePlan
	servicesCaching     bool
	servicesBootstrap   bool
	serviceCacheOutputs []backend.CacheOutputVolumeSpec
}

type dependencyStageResolveResult struct {
	completed          *cleanroomv1.CreateSandboxResponse
	restored           *cleanroomv1.CreateSandboxResponse
	sourceKind         string
	replacedDependency *cachestore.Record
	replacedServices   *cachestore.Record
}

func (s *Service) resolveServicesStageCache(ctx context.Context, req servicesStageResolveRequest) (servicesStageResolveResult, error) {
	emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_SERVICES_STAGE_CACHE, "checking services stage cache")
	var record cachestore.Record
	var found bool
	var lookupReason string
	err := s.traceCreateSandboxPhase(ctx, req.lookupTraceName(), cachePhaseAttributes(
		observability.CacheStageServices,
		observability.CacheOperationLookup,
		req.repository,
		attribute.String(observability.AttrBackend, req.backendName),
	), func(ctx context.Context) error {
		var lookupErr error
		record, found, lookupReason, lookupErr = s.lookupServicesStageCache(ctx, req.backendName, req.compiled, req.repository, req.changeset, req.plan)
		setCacheLookupSpanAttributes(ctx, found, lookupReason, lookupErr)
		return lookupErr
	})
	if err != nil {
		s.logServicesStageWarning(req.lookupWarning(), "", err)
		return servicesStageResolveResult{}, nil
	}

	if !found {
		emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_SERVICES_STAGE_CACHE, "checking services stage cache peers")
		importErr := s.traceCreateSandboxPhase(ctx, req.importTraceName(), cachePhaseAttributes(
			observability.CacheStageServices,
			observability.CacheOperationLookup,
			req.repository,
			attribute.String(observability.AttrBackend, req.backendName),
		), func(ctx context.Context) error {
			var imported bool
			var err error
			record, imported, err = s.importServicesStageCacheFromPeers(ctx, req.snapshotAdapter, req.backendName, req.compiled, req.firecrackerCfg, req.repository, req.changeset, req.plan)
			found = imported
			reason := lookupReason
			if imported {
				reason = ""
			}
			setCacheLookupSpanAttributes(ctx, imported, reason, err)
			return err
		})
		if importErr != nil {
			s.logServicesStageWarning(req.importWarning(), "", importErr)
		}
	}

	if !found {
		if !req.afterDependency {
			s.logServicesStageCacheMiss(req.backendName, req.plan.CacheKey)
			emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_SERVICES_STAGE_CACHE, "services stage cache miss")
		}
		return servicesStageResolveResult{}, nil
	}

	s.logServicesStageCacheHit(record)
	emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_SERVICES_STAGE_CACHE, "services stage cache hit")
	emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_RESTORE_SERVICES_STAGE_CACHE, "restoring services stage cache")
	restoreReq := &cleanroomv1.CreateSandboxRequest{
		Backend: req.backendName,
		Options: req.options,
	}
	var restoreResp *cleanroomv1.CreateSandboxResponse
	restoreErr := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.restore_services_stage_cache", cachePhaseAttributes(
		observability.CacheStageServices,
		observability.CacheOperationRestore,
		req.repository,
		attribute.String(observability.AttrBackend, req.backendName),
	), func(ctx context.Context) error {
		var err error
		restoreResp, err = s.createSandboxFromCacheRecord(ctx, restoreReq, req.compiled, record, nil, req.reporter)
		setCacheResultSpanAttribute(ctx, map[bool]string{true: observability.CacheResultFailed, false: observability.CacheResultRestored}[err != nil])
		return err
	})
	if restoreErr == nil {
		if cacheStore, err := s.cacheStoreOrErr(); err == nil {
			if err := cacheStore.Touch(ctx, record.Stage, record.CacheKey); err != nil {
				s.logServicesStageWarning("touch services stage cache", "", err)
			}
		}
		s.retainRestoredSandboxRepositoryState(restoreResp, req.repository, req.commitBundle, req.changeset)
		s.logServicesStageRestore(record, restoreResp.GetSandbox().GetSandboxId())
		return servicesStageResolveResult{restored: restoreResp}, nil
	}
	if errors.Is(restoreErr, errSandboxCreateAborted) {
		return servicesStageResolveResult{}, restoreErr
	}
	recordCopy := record
	s.logServicesStageRestoreWarning(record, restoreErr)
	return servicesStageResolveResult{replaced: &recordCopy}, nil
}

func (s *Service) resolveServicesStageCacheAfterDependency(ctx context.Context, req servicesStageResolveRequest) (servicesStageResolveResult, error) {
	req.afterDependency = true
	return s.resolveServicesStageCache(ctx, req)
}

func (s *Service) resolveDependencyStageCache(ctx context.Context, req dependencyStageResolveRequest) (dependencyStageResolveResult, error) {
	emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_DEPENDENCY_STAGE_CACHE, "checking dependency stage cache")
	var record cachestore.Record
	var found bool
	var lookupReason string
	err := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.lookup_dependency_stage_cache", cachePhaseAttributes(
		observability.CacheStageDependency,
		observability.CacheOperationLookup,
		req.repository,
		attribute.String(observability.AttrBackend, req.backendName),
	), func(ctx context.Context) error {
		var lookupErr error
		record, found, lookupReason, lookupErr = s.lookupDependencyStageCache(ctx, req.backendName, req.compiled, req.repository, req.changeset, req.plan)
		setCacheLookupSpanAttributes(ctx, found, lookupReason, lookupErr)
		return lookupErr
	})
	if err != nil {
		s.logDependencyStageWarning("lookup dependency stage cache", "", err)
		return dependencyStageResolveResult{}, nil
	}

	if !found {
		emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_DEPENDENCY_STAGE_CACHE, "checking dependency stage cache peers")
		importErr := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.import_dependency_stage_cache", cachePhaseAttributes(
			observability.CacheStageDependency,
			observability.CacheOperationLookup,
			req.repository,
			attribute.String(observability.AttrBackend, req.backendName),
		), func(ctx context.Context) error {
			var imported bool
			var err error
			record, imported, err = s.importDependencyStageCacheFromPeers(ctx, req.snapshotAdapter, req.backendName, req.compiled, req.firecrackerCfg, req.repository, req.changeset, req.plan)
			found = imported
			reason := lookupReason
			if imported {
				reason = ""
			}
			setCacheLookupSpanAttributes(ctx, imported, reason, err)
			return err
		})
		if importErr != nil {
			s.logDependencyStageWarning("import dependency stage cache from peer", "", importErr)
		}
	}

	if !found {
		s.logDependencyStageCacheMiss(req.backendName, req.plan.CacheKey)
		emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_DEPENDENCY_STAGE_CACHE, "dependency stage cache miss")
		return dependencyStageResolveResult{}, nil
	}

	result := dependencyStageResolveResult{}
	if req.servicesCaching {
		servicesHit, err := s.resolveServicesStageCacheAfterDependency(ctx, servicesStageResolveRequest{
			stageCacheResolveContext: req.stageCacheResolveContext,
			plan:                     req.servicesPlan,
		})
		if err != nil {
			return dependencyStageResolveResult{}, err
		}
		if servicesHit.restored != nil {
			result.completed = servicesHit.restored
			result.sourceKind = "services stage cache"
			return result, nil
		}
		result.replacedServices = servicesHit.replaced
	}

	s.logDependencyStageCacheHit(record)
	emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_DEPENDENCY_STAGE_CACHE, "dependency stage cache hit")
	emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_RESTORE_DEPENDENCY_STAGE_CACHE, "restoring dependency stage cache")
	restoreReq := &cleanroomv1.CreateSandboxRequest{
		Backend: req.backendName,
		Options: req.options,
	}
	var restoreResp *cleanroomv1.CreateSandboxResponse
	restoreErr := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.restore_dependency_stage_cache", cachePhaseAttributes(
		observability.CacheStageDependency,
		observability.CacheOperationRestore,
		req.repository,
		attribute.String(observability.AttrBackend, req.backendName),
	), func(ctx context.Context) error {
		var err error
		restoreResp, err = s.createSandboxFromCacheRecord(ctx, restoreReq, req.compiled, record, req.serviceCacheOutputs, req.reporter)
		setCacheResultSpanAttribute(ctx, map[bool]string{true: observability.CacheResultFailed, false: observability.CacheResultRestored}[err != nil])
		return err
	})
	if restoreErr == nil {
		if cacheStore, err := s.cacheStoreOrErr(); err == nil {
			if err := cacheStore.Touch(ctx, record.Stage, record.CacheKey); err != nil {
				s.logDependencyStageWarning("touch dependency stage cache", "", err)
			}
		}
		s.retainRestoredSandboxRepositoryState(restoreResp, req.repository, req.commitBundle, req.changeset)
		s.logDependencyStageRestore(record, restoreResp.GetSandbox().GetSandboxId())
		result.sourceKind = "dependency stage cache"
		if !req.servicesBootstrap {
			result.completed = restoreResp
			return result, nil
		}
		result.restored = restoreResp
		return result, nil
	}
	if errors.Is(restoreErr, errSandboxCreateAborted) {
		return dependencyStageResolveResult{}, restoreErr
	}
	recordCopy := record
	s.logDependencyStageRestoreWarning(record, restoreErr)
	result.replacedDependency = &recordCopy
	return result, nil
}

func (r servicesStageResolveRequest) lookupTraceName() string {
	if r.afterDependency {
		return "cleanroom.sandbox.lookup_services_stage_cache_after_dependency"
	}
	return "cleanroom.sandbox.lookup_services_stage_cache"
}

func (r servicesStageResolveRequest) importTraceName() string {
	if r.afterDependency {
		return "cleanroom.sandbox.import_services_stage_cache_after_dependency"
	}
	return "cleanroom.sandbox.import_services_stage_cache"
}

func (r servicesStageResolveRequest) lookupWarning() string {
	if r.afterDependency {
		return "lookup services stage cache after dependency stage cache"
	}
	return "lookup services stage cache"
}

func (r servicesStageResolveRequest) importWarning() string {
	if r.afterDependency {
		return "import services stage cache from peer after dependency stage cache"
	}
	return "import services stage cache from peer"
}
