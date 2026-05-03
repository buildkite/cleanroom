package controlservice

import (
	"context"
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/cachekey"
	"github.com/buildkite/cleanroom/internal/cachestore"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorybundle"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	serviceVolumeStageName           = "service-volume"
	serviceVolumeProducerVersion     = "cleanroom/service-volume-v1"
	serviceVolumeOutputLayoutVersion = "aggregate-v1"
)

type serviceBlockVolumePlan struct {
	ReuseNamespace       string
	DependencyOutputKeys []string
	Blocks               []serviceBlockVolumeBlockPlan
}

type serviceBlockVolumeBlockPlan struct {
	BlockName                    string
	Command                      []string
	Env                          map[string]string
	Inputs                       []string
	Outputs                      policy.StageBlockOutputs
	CacheKey                     string
	CommandDigest                string
	EnvDigest                    string
	InputManifestDigest          string
	NormalizedOutputsDigest      string
	DependencyOutputKeysDigest   string
	PriorServiceOutputKeysDigest string
	OutputVolumeLayoutVersion    string
	ProducerVersion              string
	CacheHit                     bool
	LookupReason                 string
	CacheRecord                  cachestore.Record
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
) {
	blockCount := 0
	if compiled != nil {
		blockCount = len(compiled.Services.Blocks)
	}
	if _, err := s.cacheStoreOrErr(); err != nil {
		s.logServiceBlockVolumeCacheFallback(backendName, blockCount, err.Error())
		return
	}

	plan, ok, err := s.finalizeServiceBlockVolumePlan(ctx, compiled, repository, changeset, commitBundle, backendName, runtimeBaseKey, dependencyPlan)
	if err != nil {
		s.logServicesStageWarning("resolve service block-volume cache keys", "", err)
		return
	}
	if !ok {
		return
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
		setServiceBlockVolumeLookupSpanAttributes(ctx, plan, lookupErr)
		return lookupErr
	})
	if err != nil {
		s.logServicesStageWarning("lookup service block-volume caches", "", err)
		return
	}
	s.logServiceBlockVolumeCacheLookup(backendName, plan)
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
	inputDigest, err := s.stageKeyFilesDigest(ctx, repository, changeset, commitBundle, block.Inputs.Files, serviceVolumeStageName+" "+block.Name)
	if err != nil {
		return serviceBlockVolumeBlockPlan{}, err
	}
	commandDigest, err := digestCanonicalJSON(block.Command)
	if err != nil {
		return serviceBlockVolumeBlockPlan{}, fmt.Errorf("digest service block %q command: %w", block.Name, err)
	}
	envDigest, err := digestCanonicalJSON(sortedEnvEntries(block.Env))
	if err != nil {
		return serviceBlockVolumeBlockPlan{}, fmt.Errorf("digest service block %q env: %w", block.Name, err)
	}
	outputsDigest, err := digestCanonicalJSON(block.Outputs)
	if err != nil {
		return serviceBlockVolumeBlockPlan{}, fmt.Errorf("digest service block %q outputs: %w", block.Name, err)
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
		CommandDigest:                commandDigest,
		EnvDigest:                    envDigest,
		InputManifestDigest:          strings.TrimSpace(inputDigest),
		NormalizedOutputsDigest:      outputsDigest,
		DependencyOutputKeysDigest:   dependencyDigest,
		PriorServiceOutputKeysDigest: priorDigest,
		OutputVolumeLayoutVersion:    serviceVolumeOutputLayoutVersion,
		ProducerVersion:              serviceVolumeProducerVersion,
	})
	if strings.TrimSpace(cacheKey) == "" {
		return serviceBlockVolumeBlockPlan{}, fmt.Errorf("service block %q produced an empty cache key", block.Name)
	}

	return serviceBlockVolumeBlockPlan{
		BlockName:                    block.Name,
		Command:                      append([]string(nil), block.Command...),
		Env:                          cloneDependencyBlockEnv(block.Env),
		Inputs:                       append([]string(nil), block.Inputs.Files...),
		Outputs:                      cloneStageBlockOutputs(block.Outputs),
		CacheKey:                     cacheKey,
		CommandDigest:                commandDigest,
		EnvDigest:                    envDigest,
		InputManifestDigest:          strings.TrimSpace(inputDigest),
		NormalizedOutputsDigest:      outputsDigest,
		DependencyOutputKeysDigest:   dependencyDigest,
		PriorServiceOutputKeysDigest: priorDigest,
		OutputVolumeLayoutVersion:    serviceVolumeOutputLayoutVersion,
		ProducerVersion:              serviceVolumeProducerVersion,
	}, nil
}

func (s *Service) lookupServiceBlockVolumeCaches(ctx context.Context, backendName string, compiled *policy.CompiledPolicy, plan serviceBlockVolumePlan) (serviceBlockVolumePlan, error) {
	if len(plan.Blocks) == 0 {
		return plan, nil
	}
	store, err := s.cacheStoreOrErr()
	if err != nil {
		return plan, nil
	}

	out := serviceBlockVolumePlan{
		ReuseNamespace:       plan.ReuseNamespace,
		DependencyOutputKeys: append([]string(nil), plan.DependencyOutputKeys...),
		Blocks:               make([]serviceBlockVolumeBlockPlan, len(plan.Blocks)),
	}
	copy(out.Blocks, plan.Blocks)
	for i := range out.Blocks {
		block := &out.Blocks[i]
		record, ok, err := store.GetReady(ctx, serviceVolumeStageName, block.CacheKey)
		if err != nil {
			return out, err
		}
		if !ok {
			block.LookupReason = observability.CacheLookupReasonRecordNotFound
			continue
		}
		if reason := serviceBlockVolumeRecordMissReason(record, backendName, compiled, *block); reason != "" {
			block.LookupReason = reason
			continue
		}
		block.CacheHit = true
		block.LookupReason = ""
		block.CacheRecord = record
	}
	return out, nil
}

func dependencyBlockVolumePlanCacheKeys(plan dependencyBlockVolumePlan) []string {
	if len(plan.Blocks) == 0 {
		return nil
	}
	keys := make([]string, 0, len(plan.Blocks))
	for _, block := range plan.Blocks {
		keys = append(keys, block.CacheKey)
	}
	return keys
}

func serviceBlockVolumePlanHitMissCounts(plan serviceBlockVolumePlan) (hits, misses int) {
	for _, block := range plan.Blocks {
		if block.CacheHit {
			hits++
			continue
		}
		misses++
	}
	return hits, misses
}

func setServiceBlockVolumeLookupSpanAttributes(ctx context.Context, plan serviceBlockVolumePlan, err error) {
	hits, misses := serviceBlockVolumePlanHitMissCounts(plan)
	result := observability.CacheResultFailed
	if err == nil {
		result = observability.CacheResultMiss
		if len(plan.Blocks) > 0 && misses == 0 {
			result = observability.CacheResultHit
		}
	}
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String(observability.AttrCacheResult, result),
		attribute.Int("cleanroom.cache.hit_count", hits),
		attribute.Int("cleanroom.cache.miss_count", misses),
	)
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
	hits, misses := serviceBlockVolumePlanHitMissCounts(plan)
	s.Logger.Debug("service block-volume cache lookup",
		observability.LogFieldBackend, backendName,
		"blocks", len(plan.Blocks),
		"hits", hits,
		"misses", misses,
		"reuse_namespace", plan.ReuseNamespace,
	)
}

func serviceBlockVolumeRecordMissReason(record cachestore.Record, backendName string, compiled *policy.CompiledPolicy, block serviceBlockVolumeBlockPlan) string {
	if strings.TrimSpace(record.Backend) != strings.TrimSpace(backendName) {
		return observability.CacheLookupReasonBackendMismatch
	}
	if compiled == nil || strings.TrimSpace(record.PolicyHash) != strings.TrimSpace(compiled.Hash) {
		return observability.CacheLookupReasonPolicyHashMismatch
	}
	if strings.TrimSpace(record.InputManifestDigest) != strings.TrimSpace(block.InputManifestDigest) ||
		strings.TrimSpace(record.CommandDigest) != strings.TrimSpace(block.CommandDigest) ||
		strings.TrimSpace(record.EnvDigest) != strings.TrimSpace(block.EnvDigest) ||
		strings.TrimSpace(record.NormalizedOutputsDigest) != strings.TrimSpace(block.NormalizedOutputsDigest) ||
		strings.TrimSpace(record.ProducerVersion) != strings.TrimSpace(block.ProducerVersion) ||
		len(record.OutputRecords) == 0 {
		return observability.CacheLookupReasonRecordNotFound
	}
	return ""
}
