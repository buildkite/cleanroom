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
	backendName     string
	snapshotAdapter backend.SnapshottingAdapter
	compiled        *policy.CompiledPolicy
	firecrackerCfg  backend.FirecrackerConfig
	repository      *repositorycheckout.Checkout
	changeset       *repositorychangeset.Changeset
	commitBundle    *repositorybundle.Bundle
	options         *cleanroomv1.SandboxOptions
	plan            servicesStagePlan
	reporter        CreateSandboxReporter
	afterDependency bool
}

type servicesStageResolveResult struct {
	restored *cleanroomv1.CreateSandboxResponse
	replaced *cachestore.Record
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
