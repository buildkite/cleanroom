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
	dependencyVolumeStageName = "dependency-volume"
	// Bump when dependency block-volume production semantics change, including
	// guest baseline environment defaults that can affect command behavior.
	dependencyVolumeProducerVersion     = "cleanroom/dependency-volume-v1"
	dependencyVolumeOutputLayoutVersion = "aggregate-v1"
)

func (s *Service) lookupDependencyBlockVolumePlanForCreateSandbox(
	ctx context.Context,
	adapter backend.Adapter,
	backendName string,
	compiled *policy.CompiledPolicy,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	commitBundle *repositorybundle.Bundle,
	runtimeBaseKey string,
) (dependencyBlockVolumePlan, bool) {
	blockCount := 0
	if compiled != nil {
		blockCount = len(compiled.Dependencies.Blocks)
	}
	decision := blockVolumeRuntimeDecisionForAdapter(adapter)
	if !decision.Enabled {
		s.logDependencyBlockVolumeCacheFallback(backendName, blockCount, decision.FallbackReason)
		return dependencyBlockVolumePlan{}, false
	}

	if _, err := s.cacheStoreOrErr(); err != nil {
		s.logDependencyBlockVolumeCacheFallback(backendName, blockCount, err.Error())
		return dependencyBlockVolumePlan{}, false
	}

	plan, ok, err := s.finalizeDependencyBlockVolumePlan(ctx, compiled, repository, changeset, commitBundle, backendName, runtimeBaseKey)
	if err != nil {
		s.logDependencyStageWarning("resolve dependency block-volume cache keys", "", err)
		return dependencyBlockVolumePlan{}, false
	}
	if !ok {
		return dependencyBlockVolumePlan{}, false
	}

	err = s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.lookup_dependency_block_volume_caches", cachePhaseAttributes(
		observability.CacheStageDependency,
		observability.CacheOperationLookup,
		repository,
		attribute.String(observability.AttrBackend, backendName),
		attribute.Int("cleanroom.cache.block_count", len(plan.Blocks)),
	), func(ctx context.Context) error {
		var lookupErr error
		plan, lookupErr = s.lookupDependencyBlockVolumeCaches(ctx, backendName, compiled, plan)
		hits, misses := dependencyBlockVolumePlanHitMissCounts(plan)
		setBlockVolumeLookupSpanAttributes(ctx, len(plan.Blocks), hits, misses, lookupErr)
		return lookupErr
	})
	if err != nil {
		s.logDependencyStageWarning("lookup dependency block-volume caches", "", err)
		return dependencyBlockVolumePlan{}, false
	}
	s.logDependencyBlockVolumeCacheLookup(backendName, plan)
	return plan, true
}

func (s *Service) finalizeDependencyBlockVolumePlan(
	ctx context.Context,
	compiled *policy.CompiledPolicy,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	commitBundle *repositorybundle.Bundle,
	backendName string,
	runtimeBaseKey string,
) (dependencyBlockVolumePlan, bool, error) {
	if compiled == nil || repository == nil || len(compiled.Dependencies.Blocks) == 0 || strings.TrimSpace(runtimeBaseKey) == "" {
		return dependencyBlockVolumePlan{}, false, nil
	}

	normalizedRepository := normalizeRepositoryCheckoutForComparison(repository)
	reuseNamespace := cachekey.ReuseNamespace("", normalizedRepository.RemoteURL)
	plan := dependencyBlockVolumePlan{ReuseNamespace: reuseNamespace}
	priorOutputKeys := make([]string, 0, len(compiled.Dependencies.Blocks))

	for _, block := range compiled.Dependencies.Blocks {
		blockPlan, err := s.finalizeDependencyBlockVolumeBlockPlan(ctx, compiled, repository, changeset, commitBundle, backendName, runtimeBaseKey, reuseNamespace, block, priorOutputKeys)
		if err != nil {
			return dependencyBlockVolumePlan{}, false, err
		}
		plan.Blocks = append(plan.Blocks, blockPlan)
		priorOutputKeys = append(priorOutputKeys, blockPlan.CacheKey)
	}

	return plan, true, nil
}

func (s *Service) finalizeDependencyBlockVolumeBlockPlan(
	ctx context.Context,
	compiled *policy.CompiledPolicy,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	commitBundle *repositorybundle.Bundle,
	backendName string,
	runtimeBaseKey string,
	reuseNamespace string,
	block policy.StageBlock,
	priorOutputKeys []string,
) (dependencyBlockVolumeBlockPlan, error) {
	blockPlan, err := s.finalizeBlockVolumeBlockPlanBase(ctx, repository, changeset, commitBundle, dependencyVolumeStageName, "dependency", block)
	if err != nil {
		return dependencyBlockVolumeBlockPlan{}, err
	}
	priorDigest, err := digestCanonicalJSON(append([]string{}, priorOutputKeys...))
	if err != nil {
		return dependencyBlockVolumeBlockPlan{}, fmt.Errorf("digest dependency block %q prior output keys: %w", block.Name, err)
	}

	cacheKey := cachekey.DependencyVolumeKey(cachekey.DependencyVolumeInputs{
		Backend:                         strings.TrimSpace(backendName),
		RuntimeKey:                      strings.TrimSpace(runtimeBaseKey),
		ReuseNamespace:                  strings.TrimSpace(reuseNamespace),
		CompiledPolicyHash:              strings.TrimSpace(compiled.Hash),
		DestinationDir:                  strings.TrimSpace(normalizeRepositoryCheckoutForComparison(repository).DestinationDir),
		RepositorySourceDigest:          blockPlan.RepositorySourceDigest,
		BlockName:                       strings.TrimSpace(block.Name),
		CommandDigest:                   blockPlan.CommandDigest,
		EnvDigest:                       blockPlan.EnvDigest,
		InputManifestDigest:             blockPlan.InputManifestDigest,
		NormalizedOutputsDigest:         blockPlan.NormalizedOutputsDigest,
		PriorDependencyOutputKeysDigest: priorDigest,
		OutputVolumeLayoutVersion:       dependencyVolumeOutputLayoutVersion,
		ProducerVersion:                 dependencyVolumeProducerVersion,
	})
	if strings.TrimSpace(cacheKey) == "" {
		return dependencyBlockVolumeBlockPlan{}, fmt.Errorf("dependency block %q produced an empty cache key", block.Name)
	}

	blockPlan.CacheKey = cacheKey
	blockPlan.PriorDependencyOutputKeysDigest = priorDigest
	blockPlan.OutputVolumeLayoutVersion = dependencyVolumeOutputLayoutVersion
	blockPlan.ProducerVersion = dependencyVolumeProducerVersion
	return dependencyBlockVolumeBlockPlan(blockPlan), nil
}

func (s *Service) lookupDependencyBlockVolumeCaches(ctx context.Context, backendName string, compiled *policy.CompiledPolicy, plan dependencyBlockVolumePlan) (dependencyBlockVolumePlan, error) {
	if len(plan.Blocks) == 0 {
		return plan, nil
	}
	store, err := s.cacheStoreOrErr()
	if err != nil {
		return plan, nil
	}

	out := dependencyBlockVolumePlan{
		ReuseNamespace: plan.ReuseNamespace,
		Blocks:         make([]dependencyBlockVolumeBlockPlan, len(plan.Blocks)),
	}
	copy(out.Blocks, plan.Blocks)
	for i := range out.Blocks {
		block, err := lookupBlockVolumeCache(ctx, store, dependencyVolumeStageName, backendName, compiled, blockVolumeBlockPlan(out.Blocks[i]))
		if err != nil {
			return out, err
		}
		out.Blocks[i] = dependencyBlockVolumeBlockPlan(block)
	}
	return out, nil
}

func dependencyBlockVolumePlanHitMissCounts(plan dependencyBlockVolumePlan) (hits, misses int) {
	for _, block := range plan.Blocks {
		if block.CacheHit {
			hits++
			continue
		}
		misses++
	}
	return hits, misses
}

func (s *Service) logDependencyBlockVolumeCacheFallback(backendName string, blockCount int, reason string) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Debug("dependency block-volume cache fallback",
		observability.LogFieldBackend, backendName,
		"blocks", blockCount,
		"reason", strings.TrimSpace(reason),
	)
}

func (s *Service) logDependencyBlockVolumeCacheLookup(backendName string, plan dependencyBlockVolumePlan) {
	if s == nil || s.Logger == nil {
		return
	}
	hits, misses := dependencyBlockVolumePlanHitMissCounts(plan)
	s.Logger.Debug("dependency block-volume cache lookup",
		observability.LogFieldBackend, backendName,
		"blocks", len(plan.Blocks),
		"hits", hits,
		"misses", misses,
		"reuse_namespace", plan.ReuseNamespace,
	)
}
