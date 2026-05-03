package controlservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
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
	dependencyVolumeStageName           = "dependency-volume"
	dependencyVolumeProducerVersion     = "cleanroom/dependency-volume-v1"
	dependencyVolumeOutputLayoutVersion = "aggregate-v1"
)

type dependencyBlockVolumePlan struct {
	ReuseNamespace string
	Blocks         []dependencyBlockVolumeBlockPlan
}

type dependencyBlockVolumeBlockPlan struct {
	BlockName                       string
	Command                         []string
	Env                             map[string]string
	Inputs                          []string
	Outputs                         policy.StageBlockOutputs
	CacheKey                        string
	CommandDigest                   string
	EnvDigest                       string
	InputManifestDigest             string
	NormalizedOutputsDigest         string
	PriorDependencyOutputKeysDigest string
	OutputVolumeLayoutVersion       string
	ProducerVersion                 string
	CacheHit                        bool
	LookupReason                    string
	CacheRecord                     cachestore.Record
}

type dependencyBlockVolumeRuntimeDecision struct {
	Enabled        bool
	FallbackReason string
}

func dependencyBlockVolumeRuntimeDecisionForAdapter(adapter backend.Adapter) dependencyBlockVolumeRuntimeDecision {
	return blockVolumeRuntimeDecisionForAdapter(adapter)
}

func blockVolumeRuntimeDecisionForAdapter(adapter backend.Adapter) dependencyBlockVolumeRuntimeDecision {
	if adapter == nil {
		return dependencyBlockVolumeRuntimeDecision{FallbackReason: "backend adapter unavailable"}
	}
	caps := backend.CapabilitiesForAdapter(adapter)
	for _, required := range []string{
		backend.CapabilitySandboxCacheOutputVolumes,
		backend.CapabilitySandboxOverlayWriteCapture,
	} {
		if !caps[required] {
			return dependencyBlockVolumeRuntimeDecision{FallbackReason: "backend missing capability " + required}
		}
	}
	return dependencyBlockVolumeRuntimeDecision{Enabled: true}
}

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
		setDependencyBlockVolumeLookupSpanAttributes(ctx, plan, lookupErr)
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
	inputDigest, err := s.stageKeyFilesDigest(ctx, repository, changeset, commitBundle, block.Inputs.Files, dependencyVolumeStageName+" "+block.Name)
	if err != nil {
		return dependencyBlockVolumeBlockPlan{}, err
	}
	commandDigest, err := digestCanonicalJSON(block.Command)
	if err != nil {
		return dependencyBlockVolumeBlockPlan{}, fmt.Errorf("digest dependency block %q command: %w", block.Name, err)
	}
	envDigest, err := digestCanonicalJSON(sortedEnvEntries(block.Env))
	if err != nil {
		return dependencyBlockVolumeBlockPlan{}, fmt.Errorf("digest dependency block %q env: %w", block.Name, err)
	}
	outputsDigest, err := digestCanonicalJSON(block.Outputs)
	if err != nil {
		return dependencyBlockVolumeBlockPlan{}, fmt.Errorf("digest dependency block %q outputs: %w", block.Name, err)
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
		BlockName:                       strings.TrimSpace(block.Name),
		CommandDigest:                   commandDigest,
		EnvDigest:                       envDigest,
		InputManifestDigest:             strings.TrimSpace(inputDigest),
		NormalizedOutputsDigest:         outputsDigest,
		PriorDependencyOutputKeysDigest: priorDigest,
		OutputVolumeLayoutVersion:       dependencyVolumeOutputLayoutVersion,
		ProducerVersion:                 dependencyVolumeProducerVersion,
	})
	if strings.TrimSpace(cacheKey) == "" {
		return dependencyBlockVolumeBlockPlan{}, fmt.Errorf("dependency block %q produced an empty cache key", block.Name)
	}

	return dependencyBlockVolumeBlockPlan{
		BlockName:                       block.Name,
		Command:                         append([]string(nil), block.Command...),
		Env:                             cloneDependencyBlockEnv(block.Env),
		Inputs:                          append([]string(nil), block.Inputs.Files...),
		Outputs:                         cloneStageBlockOutputs(block.Outputs),
		CacheKey:                        cacheKey,
		CommandDigest:                   commandDigest,
		EnvDigest:                       envDigest,
		InputManifestDigest:             strings.TrimSpace(inputDigest),
		NormalizedOutputsDigest:         outputsDigest,
		PriorDependencyOutputKeysDigest: priorDigest,
		OutputVolumeLayoutVersion:       dependencyVolumeOutputLayoutVersion,
		ProducerVersion:                 dependencyVolumeProducerVersion,
	}, nil
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
		block := &out.Blocks[i]
		record, ok, err := store.GetReady(ctx, dependencyVolumeStageName, block.CacheKey)
		if err != nil {
			return out, err
		}
		if !ok {
			block.LookupReason = observability.CacheLookupReasonRecordNotFound
			continue
		}
		if reason := dependencyBlockVolumeRecordMissReason(record, backendName, compiled, *block); reason != "" {
			block.LookupReason = reason
			continue
		}
		block.CacheHit = true
		block.LookupReason = ""
		block.CacheRecord = record
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

func setDependencyBlockVolumeLookupSpanAttributes(ctx context.Context, plan dependencyBlockVolumePlan, err error) {
	hits, misses := dependencyBlockVolumePlanHitMissCounts(plan)
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

func dependencyBlockVolumeRecordMissReason(record cachestore.Record, backendName string, compiled *policy.CompiledPolicy, block dependencyBlockVolumeBlockPlan) string {
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

type envEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func sortedEnvEntries(env map[string]string) []envEntry {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]envEntry, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, envEntry{Name: key, Value: env[key]})
	}
	return entries
}

func cloneStageBlockOutputs(outputs policy.StageBlockOutputs) policy.StageBlockOutputs {
	return policy.StageBlockOutputs{
		Dirs:  append([]string(nil), outputs.Dirs...),
		Files: append([]string(nil), outputs.Files...),
	}
}

func cloneDependencyBlockEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for key, value := range env {
		out[key] = value
	}
	return out
}

func digestCanonicalJSON(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
