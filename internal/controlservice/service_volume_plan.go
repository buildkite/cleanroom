package controlservice

import (
	"context"
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachekey"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorybundle"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"go.opentelemetry.io/otel/attribute"
)

const (
	serviceVolumeStageName           = "service-volume"
	serviceVolumeProducerVersion     = "cleanroom/service-volume-v1"
	serviceVolumeOutputLayoutVersion = "aggregate-v1"
)

func (s *Service) maybeLookupServiceBlockVolumePlanForCreateSandbox(
	ctx context.Context,
	adapter backend.Adapter,
	backendName string,
	compiled *policy.CompiledPolicy,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	commitBundle *repositorybundle.Bundle,
	runtimeBaseKey string,
	dependencyStageBootstrapEnabled bool,
	dependencyPlan dependencyBlockVolumePlan,
	dependencyPlanAvailable bool,
) (serviceBlockVolumePlan, bool) {
	blockCount := 0
	if compiled != nil {
		blockCount = len(compiled.Services.Blocks)
	}
	decision := blockVolumeRuntimeDecisionForAdapter(adapter)
	if !decision.Enabled {
		s.logServiceBlockVolumeCacheFallback(backendName, blockCount, decision.FallbackReason)
		return serviceBlockVolumePlan{}, false
	}
	if dependencyStageBootstrapEnabled && !dependencyPlanAvailable {
		s.logServiceBlockVolumeCacheFallback(backendName, blockCount, "dependency block-volume plan unavailable")
		return serviceBlockVolumePlan{}, false
	}
	return s.lookupServiceBlockVolumePlanForCreateSandbox(ctx, backendName, compiled, repository, changeset, commitBundle, runtimeBaseKey, dependencyPlan)
}

func (s *Service) lookupServiceBlockVolumePlanForCreateSandbox(
	ctx context.Context,
	backendName string,
	compiled *policy.CompiledPolicy,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	commitBundle *repositorybundle.Bundle,
	runtimeBaseKey string,
	dependencyPlan dependencyBlockVolumePlan,
) (serviceBlockVolumePlan, bool) {
	blockCount := 0
	if compiled != nil {
		blockCount = len(compiled.Services.Blocks)
	}
	if _, err := s.cacheStoreOrErr(); err != nil {
		s.logServiceBlockVolumeCacheFallback(backendName, blockCount, err.Error())
		return serviceBlockVolumePlan{}, false
	}

	plan, ok, err := s.finalizeServiceBlockVolumePlan(ctx, compiled, repository, changeset, commitBundle, backendName, runtimeBaseKey, dependencyPlan)
	if err != nil {
		s.logServicesStageWarning("resolve service block-volume cache keys", "", err)
		return serviceBlockVolumePlan{}, false
	}
	if !ok {
		return serviceBlockVolumePlan{}, false
	}

	err = s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.lookup_service_block_volume_caches", cachePhaseAttributes(
		observability.CacheStageServices,
		observability.CacheOperationLookup,
		repository,
		attribute.String(observability.AttrBackend, backendName),
		attribute.Int("cleanroom.cache.block_count", len(plan.Blocks)),
	), func(ctx context.Context) error {
		var lookupErr error
		plan, lookupErr = s.lookupServiceBlockVolumeCaches(ctx, backendName, compiled, plan)
		setBlockVolumeLookupSpanAttributes(ctx, blockVolumePlan(plan), lookupErr)
		return lookupErr
	})
	if err != nil {
		s.logServicesStageWarning("lookup service block-volume caches", "", err)
		return serviceBlockVolumePlan{}, false
	}
	s.logServiceBlockVolumeCacheLookup(backendName, plan)
	return plan, true
}

func (s *Service) finalizeServiceBlockVolumePlan(
	ctx context.Context,
	compiled *policy.CompiledPolicy,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	commitBundle *repositorybundle.Bundle,
	backendName string,
	runtimeBaseKey string,
	dependencyPlan dependencyBlockVolumePlan,
) (serviceBlockVolumePlan, bool, error) {
	if compiled == nil || repository == nil || len(compiled.Services.Blocks) == 0 || strings.TrimSpace(runtimeBaseKey) == "" {
		return serviceBlockVolumePlan{}, false, nil
	}

	normalizedRepository := normalizeRepositoryCheckoutForComparison(repository)
	reuseNamespace := cachekey.ReuseNamespace("", normalizedRepository.RemoteURL)
	dependencyOutputKeys := dependencyBlockVolumePlanCacheKeys(dependencyPlan)
	plan := serviceBlockVolumePlan{
		ReuseNamespace:       reuseNamespace,
		DependencyOutputKeys: dependencyOutputKeys,
	}
	priorServiceOutputKeys := make([]string, 0, len(compiled.Services.Blocks))

	for _, block := range compiled.Services.Blocks {
		blockPlan, err := s.finalizeServiceBlockVolumeBlockPlan(ctx, compiled, repository, changeset, commitBundle, backendName, runtimeBaseKey, reuseNamespace, dependencyOutputKeys, block, priorServiceOutputKeys)
		if err != nil {
			return serviceBlockVolumePlan{}, false, err
		}
		plan.Blocks = append(plan.Blocks, blockPlan)
		priorServiceOutputKeys = append(priorServiceOutputKeys, blockPlan.CacheKey)
	}

	return plan, true, nil
}

func (s *Service) finalizeServiceBlockVolumeBlockPlan(
	ctx context.Context,
	compiled *policy.CompiledPolicy,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	commitBundle *repositorybundle.Bundle,
	backendName string,
	runtimeBaseKey string,
	reuseNamespace string,
	dependencyOutputKeys []string,
	block policy.StageBlock,
	priorServiceOutputKeys []string,
) (serviceBlockVolumeBlockPlan, error) {
	blockPlan, err := s.finalizeBlockVolumeBlockPlanBase(ctx, repository, changeset, commitBundle, serviceVolumeStageName, "service", block)
	if err != nil {
		return serviceBlockVolumeBlockPlan{}, err
	}
	dependencyDigest, err := digestCanonicalJSON(append([]string{}, dependencyOutputKeys...))
	if err != nil {
		return serviceBlockVolumeBlockPlan{}, fmt.Errorf("digest service block %q dependency output keys: %w", block.Name, err)
	}
	priorDigest, err := digestCanonicalJSON(append([]string{}, priorServiceOutputKeys...))
	if err != nil {
		return serviceBlockVolumeBlockPlan{}, fmt.Errorf("digest service block %q prior output keys: %w", block.Name, err)
	}

	cacheKey := cachekey.ServiceVolumeKey(cachekey.ServiceVolumeInputs{
		Backend:                      strings.TrimSpace(backendName),
		RuntimeKey:                   strings.TrimSpace(runtimeBaseKey),
		ReuseNamespace:               strings.TrimSpace(reuseNamespace),
		CompiledPolicyHash:           strings.TrimSpace(compiled.Hash),
		BlockName:                    strings.TrimSpace(block.Name),
		CommandDigest:                blockPlan.CommandDigest,
		EnvDigest:                    blockPlan.EnvDigest,
		InputManifestDigest:          blockPlan.InputManifestDigest,
		NormalizedOutputsDigest:      blockPlan.NormalizedOutputsDigest,
		DependencyOutputKeysDigest:   dependencyDigest,
		PriorServiceOutputKeysDigest: priorDigest,
		OutputVolumeLayoutVersion:    serviceVolumeOutputLayoutVersion,
		ProducerVersion:              serviceVolumeProducerVersion,
	})
	if strings.TrimSpace(cacheKey) == "" {
		return serviceBlockVolumeBlockPlan{}, fmt.Errorf("service block %q produced an empty cache key", block.Name)
	}

	blockPlan.CacheKey = cacheKey
	blockPlan.DependencyOutputKeysDigest = dependencyDigest
	blockPlan.PriorServiceOutputKeysDigest = priorDigest
	blockPlan.OutputVolumeLayoutVersion = serviceVolumeOutputLayoutVersion
	blockPlan.ProducerVersion = serviceVolumeProducerVersion
	return blockPlan, nil
}

func (s *Service) lookupServiceBlockVolumeCaches(ctx context.Context, backendName string, compiled *policy.CompiledPolicy, plan serviceBlockVolumePlan) (serviceBlockVolumePlan, error) {
	out, err := s.lookupBlockVolumeCaches(ctx, serviceVolumeStageName, backendName, compiled, blockVolumePlan(plan))
	return serviceBlockVolumePlan(out), err
}

func (s *Service) logServiceBlockVolumeCacheFallback(backendName string, blockCount int, reason string) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Debug("service block-volume cache fallback",
		observability.LogFieldBackend, backendName,
		"blocks", blockCount,
		"reason", strings.TrimSpace(reason),
	)
}

func (s *Service) logServiceBlockVolumeCacheLookup(backendName string, plan serviceBlockVolumePlan) {
	if s == nil || s.Logger == nil {
		return
	}
	hits, misses := blockVolumePlanHitMissCounts(blockVolumePlan(plan))
	s.Logger.Debug("service block-volume cache lookup",
		observability.LogFieldBackend, backendName,
		"blocks", len(plan.Blocks),
		"hits", hits,
		"misses", misses,
		"reuse_namespace", plan.ReuseNamespace,
	)
}
