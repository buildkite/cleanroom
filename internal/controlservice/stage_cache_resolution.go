package controlservice

import (
	"context"
	"errors"
	"strings"

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

type stageCacheResolveRequest struct {
	stageCacheResolveContext
	adapter                      backend.Adapter
	workspaceStageRuntimeBaseKey string
	workspaceStageCacheKey       string
	dependencyStagePlan          dependencyStagePlan
	dependencyStageCaching       bool
	dependencyStageBootstrap     bool
	dependencyCacheOutputVolumes []backend.CacheOutputVolumeSpec
	servicesStagePlan            servicesStagePlan
	servicesStageCaching         bool
	servicesStageBootstrap       bool
	servicesCacheOutputVolumes   []backend.CacheOutputVolumeSpec
}

type stageCacheResolveResult struct {
	completed          *cleanroomv1.CreateSandboxResponse
	restoredWorkspace  *cleanroomv1.CreateSandboxResponse
	restoredDependency *cleanroomv1.CreateSandboxResponse
	sourceKind         string
	replacedWorkspace  *cachestore.Record
	replacedDependency *cachestore.Record
	replacedServices   *cachestore.Record
}

type dependencyStageResolveRequest struct {
	stageCacheResolveContext
	plan                dependencyStagePlan
	adapter             backend.Adapter
	servicesBootstrap   bool
	serviceCacheOutputs []backend.CacheOutputVolumeSpec
}

type dependencyStageResolveResult struct {
	completed          *cleanroomv1.CreateSandboxResponse
	restored           *cleanroomv1.CreateSandboxResponse
	sourceKind         string
	replacedDependency *cachestore.Record
}

type workspaceStageResolveRequest struct {
	stageCacheResolveContext
	runtimeBaseKey      string
	cacheKey            string
	dependencyBootstrap bool
	servicesBootstrap   bool
	cacheOutputs        []backend.CacheOutputVolumeSpec
}

type workspaceStageResolveResult struct {
	completed  *cleanroomv1.CreateSandboxResponse
	restored   *cleanroomv1.CreateSandboxResponse
	sourceKind string
	replaced   *cachestore.Record
}

type dependencyStageLookupResult struct {
	record cachestore.Record
	found  bool
}

func (s *Service) resolveStageCaches(ctx context.Context, req stageCacheResolveRequest) (stageCacheResolveResult, error) {
	result := stageCacheResolveResult{}

	if req.servicesStageCaching {
		servicesHit, err := s.resolveServicesStageCache(ctx, servicesStageResolveRequest{
			stageCacheResolveContext: req.stageCacheResolveContext,
			plan:                     req.servicesStagePlan,
		})
		if err != nil {
			return stageCacheResolveResult{}, err
		}
		if servicesHit.restored != nil {
			result.completed = servicesHit.restored
			result.sourceKind = "services stage cache"
			return result, nil
		}
		result.replacedServices = servicesHit.replaced
	}

	if req.dependencyStageCaching {
		dependencyReq := dependencyStageResolveRequest{
			stageCacheResolveContext: req.stageCacheResolveContext,
			plan:                     req.dependencyStagePlan,
			adapter:                  req.adapter,
			servicesBootstrap:        req.servicesStageBootstrap,
			serviceCacheOutputs:      req.servicesCacheOutputVolumes,
		}
		dependencyLookup, err := s.lookupDependencyStageCacheForResolution(ctx, dependencyReq)
		if err != nil {
			return stageCacheResolveResult{}, err
		}

		if dependencyLookup.found {
			if req.servicesStageCaching {
				servicesHit, err := s.resolveServicesStageCacheAfterDependency(ctx, servicesStageResolveRequest{
					stageCacheResolveContext: req.stageCacheResolveContext,
					plan:                     req.servicesStagePlan,
				})
				if err != nil {
					return stageCacheResolveResult{}, err
				}
				if servicesHit.restored != nil {
					result.completed = servicesHit.restored
					result.sourceKind = "services stage cache"
					return result, nil
				}
				if servicesHit.replaced != nil {
					result.replacedServices = servicesHit.replaced
				}
			}

			dependencyHit, err := s.restoreDependencyStageCache(ctx, dependencyReq, dependencyLookup.record)
			if err != nil {
				return stageCacheResolveResult{}, err
			}
			if dependencyHit.replacedDependency != nil {
				result.replacedDependency = dependencyHit.replacedDependency
			}
			if dependencyHit.completed != nil {
				result.completed = dependencyHit.completed
				result.sourceKind = dependencyHit.sourceKind
				return result, nil
			}
			if dependencyHit.restored != nil {
				result.restoredDependency = dependencyHit.restored
				result.sourceKind = dependencyHit.sourceKind
				return result, nil
			}
		}

		portableHit, err := s.resolvePortableDependencyStageCache(ctx, dependencyReq)
		if err != nil {
			return stageCacheResolveResult{}, err
		}
		if portableHit.completed != nil {
			result.completed = portableHit.completed
			result.sourceKind = portableHit.sourceKind
			return result, nil
		}
		if portableHit.restored != nil {
			result.restoredDependency = portableHit.restored
			result.sourceKind = portableHit.sourceKind
			return result, nil
		}
	}

	workspaceHit, err := s.resolveWorkspaceStageCache(ctx, workspaceStageResolveRequest{
		stageCacheResolveContext: req.stageCacheResolveContext,
		runtimeBaseKey:           req.workspaceStageRuntimeBaseKey,
		cacheKey:                 req.workspaceStageCacheKey,
		dependencyBootstrap:      req.dependencyStageBootstrap,
		servicesBootstrap:        req.servicesStageBootstrap,
		cacheOutputs:             appendCacheOutputVolumeSpecs(req.dependencyCacheOutputVolumes, req.servicesCacheOutputVolumes),
	})
	if err != nil {
		return stageCacheResolveResult{}, err
	}
	if workspaceHit.replaced != nil {
		result.replacedWorkspace = workspaceHit.replaced
	}
	if workspaceHit.completed != nil {
		result.completed = workspaceHit.completed
		result.sourceKind = workspaceHit.sourceKind
		return result, nil
	}
	if workspaceHit.restored != nil {
		result.restoredWorkspace = workspaceHit.restored
		result.sourceKind = workspaceHit.sourceKind
	}
	return result, nil
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
			if err := touchCacheRecord(ctx, cacheStore, record); err != nil {
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

func (s *Service) lookupDependencyStageCacheForResolution(ctx context.Context, req dependencyStageResolveRequest) (dependencyStageLookupResult, error) {
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
		return dependencyStageLookupResult{}, nil
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
		return dependencyStageLookupResult{}, nil
	}

	return dependencyStageLookupResult{record: record, found: true}, nil
}

func (s *Service) restoreDependencyStageCache(ctx context.Context, req dependencyStageResolveRequest, record cachestore.Record) (dependencyStageResolveResult, error) {
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
			if err := touchCacheRecord(ctx, cacheStore, record); err != nil {
				s.logDependencyStageWarning("touch dependency stage cache", "", err)
			}
		}
		s.retainRestoredSandboxRepositoryState(restoreResp, req.repository, req.commitBundle, req.changeset)
		s.logDependencyStageRestore(record, restoreResp.GetSandbox().GetSandboxId())
		result := dependencyStageResolveResult{sourceKind: "dependency stage cache"}
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
	return dependencyStageResolveResult{replacedDependency: &recordCopy}, nil
}

func (s *Service) resolvePortableDependencyStageCache(ctx context.Context, req dependencyStageResolveRequest) (dependencyStageResolveResult, error) {
	if strings.TrimSpace(req.plan.PortableCacheKey) == "" {
		return dependencyStageResolveResult{}, nil
	}

	emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_DEPENDENCY_STAGE_CACHE, "checking portable dependency stage cache")
	var record cachestore.Record
	var found bool
	var lookupReason string
	err := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.lookup_portable_dependency_stage_cache", cachePhaseAttributes(
		observability.CacheStageDependency,
		observability.CacheOperationLookup,
		req.repository,
		attribute.String(observability.AttrBackend, req.backendName),
	), func(ctx context.Context) error {
		var lookupErr error
		record, found, lookupReason, lookupErr = s.lookupPortableDependencyStageCache(ctx, req.backendName, req.compiled, req.repository, req.plan)
		setCacheLookupSpanAttributes(ctx, found, lookupReason, lookupErr)
		return lookupErr
	})
	if err != nil {
		s.logDependencyStageWarning("lookup portable dependency stage cache", "", err)
		return dependencyStageResolveResult{}, nil
	}

	if !found {
		s.logDependencyStageCacheMiss(req.backendName, req.plan.PortableCacheKey)
		emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_DEPENDENCY_STAGE_CACHE, "portable dependency stage cache miss")
		return dependencyStageResolveResult{}, nil
	}

	s.logDependencyStageCacheHit(record)
	emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_DEPENDENCY_STAGE_CACHE, "portable dependency stage cache hit")
	var restoreResp *cleanroomv1.CreateSandboxResponse
	restoreErr := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.restore_portable_dependency_stage_cache", cachePhaseAttributes(
		observability.CacheStageDependency,
		observability.CacheOperationRestore,
		req.repository,
		attribute.String(observability.AttrBackend, req.backendName),
	), func(ctx context.Context) error {
		var err error
		restoreResp, err = s.restorePortableDependencyStageCache(ctx, req.adapter, req.backendName, req.compiled, req.firecrackerCfg, req.repository, req.changeset, req.commitBundle, req.options, req.plan, record, req.serviceCacheOutputs, req.reporter)
		setCacheResultSpanAttribute(ctx, map[bool]string{true: observability.CacheResultFailed, false: observability.CacheResultRestored}[err != nil])
		return err
	})
	if restoreErr != nil {
		if errors.Is(restoreErr, errSandboxCreateAborted) {
			return dependencyStageResolveResult{}, restoreErr
		}
		s.logDependencyStageRestoreWarning(record, restoreErr)
		return dependencyStageResolveResult{}, nil
	}

	if cacheStore, err := s.cacheStoreOrErr(); err == nil {
		if err := touchCacheRecord(ctx, cacheStore, record); err != nil {
			s.logDependencyStageWarning("touch portable dependency stage cache", "", err)
		}
	}
	s.logDependencyStageRestore(record, restoreResp.GetSandbox().GetSandboxId())
	result := dependencyStageResolveResult{
		sourceKind: "portable dependency stage cache",
	}
	if !req.servicesBootstrap {
		result.completed = restoreResp
		return result, nil
	}
	result.restored = restoreResp
	return result, nil
}

func (s *Service) resolveWorkspaceStageCache(ctx context.Context, req workspaceStageResolveRequest) (workspaceStageResolveResult, error) {
	emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_WORKSPACE_STAGE_CACHE, "checking workspace stage cache")
	var record cachestore.Record
	var found bool
	var lookupReason string
	err := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.lookup_workspace_stage_cache", cachePhaseAttributes(
		observability.CacheStageWorkspace,
		observability.CacheOperationLookup,
		req.repository,
		attribute.String(observability.AttrBackend, req.backendName),
	), func(ctx context.Context) error {
		var lookupErr error
		record, found, lookupReason, lookupErr = s.lookupWorkspaceStageCache(ctx, req.backendName, req.compiled, req.runtimeBaseKey, req.repository, req.changeset)
		setCacheLookupSpanAttributes(ctx, found, lookupReason, lookupErr)
		return lookupErr
	})
	if err != nil {
		s.logWorkspaceStageWarning("lookup workspace stage cache", "", err)
		return workspaceStageResolveResult{}, nil
	}

	if !found {
		s.logWorkspaceStageCacheMiss(req.backendName, req.cacheKey)
		emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_WORKSPACE_STAGE_CACHE, "workspace stage cache miss")
		return workspaceStageResolveResult{}, nil
	}

	s.logWorkspaceStageCacheHit(record)
	emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_WORKSPACE_STAGE_CACHE, "workspace stage cache hit")
	emitCreateSandboxMessage(req.reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_RESTORE_WORKSPACE_STAGE_CACHE, "restoring workspace stage cache")
	restoreReq := &cleanroomv1.CreateSandboxRequest{
		Backend: req.backendName,
		Options: req.options,
	}
	var restoreResp *cleanroomv1.CreateSandboxResponse
	restoreErr := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.restore_workspace_stage_cache", cachePhaseAttributes(
		observability.CacheStageWorkspace,
		observability.CacheOperationRestore,
		req.repository,
		attribute.String(observability.AttrBackend, req.backendName),
	), func(ctx context.Context) error {
		var err error
		restoreResp, err = s.createSandboxFromCacheRecord(ctx, restoreReq, req.compiled, record, req.cacheOutputs, req.reporter)
		setCacheResultSpanAttribute(ctx, map[bool]string{true: observability.CacheResultFailed, false: observability.CacheResultRestored}[err != nil])
		return err
	})
	if restoreErr == nil {
		if cacheStore, err := s.cacheStoreOrErr(); err == nil {
			if err := touchCacheRecord(ctx, cacheStore, record); err != nil {
				s.logWorkspaceStageWarning("touch workspace stage cache", "", err)
			}
		}
		s.retainRestoredSandboxRepositoryState(restoreResp, req.repository, req.commitBundle, req.changeset)
		s.logWorkspaceStageRestore(record, restoreResp.GetSandbox().GetSandboxId())
		result := workspaceStageResolveResult{sourceKind: "workspace stage cache"}
		if !req.dependencyBootstrap && !req.servicesBootstrap {
			result.completed = restoreResp
			return result, nil
		}
		result.restored = restoreResp
		return result, nil
	}
	if errors.Is(restoreErr, errSandboxCreateAborted) {
		return workspaceStageResolveResult{}, restoreErr
	}
	recordCopy := record
	s.logWorkspaceStageRestoreWarning(record, restoreErr)
	return workspaceStageResolveResult{replaced: &recordCopy}, nil
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
