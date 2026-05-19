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
	blockVolumeAdaptiveHeadroomBytes   int64 = 1 << 30
	blockVolumeAdaptiveHeadroomDivisor int64 = 4
	blockVolumeMaxInt64                int64 = 9223372036854775807
)

type blockVolumeBlockPlan struct {
	BlockName                       string
	Command                         []string
	Env                             map[string]string
	Inputs                          []string
	Outputs                         policy.StageBlockOutputs
	CacheKey                        string
	CommandDigest                   string
	EnvDigest                       string
	InputManifestDigest             string
	RepositorySourceDigest          string
	NormalizedOutputsDigest         string
	DependencyOutputKeysDigest      string
	PriorDependencyOutputKeysDigest string
	PriorServiceOutputKeysDigest    string
	OutputVolumeLayoutVersion       string
	ProducerVersion                 string
	CacheOutputMinimumBytes         int64
	CacheHit                        bool
	LookupReason                    string
	CacheRecord                     cachestore.Record
}

type dependencyBlockVolumePlan struct {
	ReuseNamespace string
	Blocks         []dependencyBlockVolumeBlockPlan
}

type dependencyBlockVolumeBlockPlan blockVolumeBlockPlan

type serviceBlockVolumePlan struct {
	ReuseNamespace       string
	DependencyOutputKeys []string
	DependencyOutputDirs []string
	Blocks               []serviceBlockVolumeBlockPlan
}

type serviceBlockVolumeBlockPlan blockVolumeBlockPlan

type blockVolumeRuntimeDecision struct {
	Enabled        bool
	FallbackReason string
}

type blockVolumeLookupSpanEventBlock struct {
	BlockName    string
	CacheKey     string
	Outputs      policy.StageBlockOutputs
	CacheHit     bool
	LookupReason string
}

type dependencyBlockVolumeRuntimeDecision = blockVolumeRuntimeDecision

func dependencyBlockVolumeRuntimeDecisionForAdapter(adapter backend.Adapter) dependencyBlockVolumeRuntimeDecision {
	return blockVolumeRuntimeDecisionForAdapter(adapter)
}

func blockVolumeRuntimeDecisionForAdapter(adapter backend.Adapter) blockVolumeRuntimeDecision {
	if adapter == nil {
		return blockVolumeRuntimeDecision{FallbackReason: "backend adapter unavailable"}
	}
	caps := backend.CapabilitiesForAdapter(adapter)
	for _, required := range []string{
		backend.CapabilitySandboxCacheOutputVolumes,
		backend.CapabilitySandboxOverlayWriteCapture,
	} {
		if !caps[required] {
			return blockVolumeRuntimeDecision{FallbackReason: "backend missing capability " + required}
		}
	}
	return blockVolumeRuntimeDecision{Enabled: true}
}

func (s *Service) finalizeBlockVolumeBlockPlanBase(
	ctx context.Context,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	commitBundle *repositorybundle.Bundle,
	stageName string,
	phaseName string,
	block policy.StageBlock,
) (blockVolumeBlockPlan, error) {
	inputDigest, err := s.stageInputFilesDigest(ctx, repository, changeset, commitBundle, block.Inputs.Files, stageName+" "+block.Name)
	if err != nil {
		return blockVolumeBlockPlan{}, err
	}
	commandDigest, err := digestCanonicalJSON(block.Command)
	if err != nil {
		return blockVolumeBlockPlan{}, fmt.Errorf("digest %s block %q command: %w", phaseName, block.Name, err)
	}
	envDigest, err := digestCanonicalJSON(sortedEnvEntries(block.Env))
	if err != nil {
		return blockVolumeBlockPlan{}, fmt.Errorf("digest %s block %q env: %w", phaseName, block.Name, err)
	}
	repositorySourceDigest, err := blockVolumeRepositorySourceDigest(repository, changeset)
	if err != nil {
		return blockVolumeBlockPlan{}, fmt.Errorf("digest %s block %q repository source: %w", phaseName, block.Name, err)
	}
	outputsDigest, err := digestCanonicalJSON(block.Outputs)
	if err != nil {
		return blockVolumeBlockPlan{}, fmt.Errorf("digest %s block %q outputs: %w", phaseName, block.Name, err)
	}

	return blockVolumeBlockPlan{
		BlockName:               block.Name,
		Command:                 append([]string(nil), block.Command...),
		Env:                     cloneBlockEnv(block.Env),
		Inputs:                  append([]string(nil), block.Inputs.Files...),
		Outputs:                 cloneStageBlockOutputs(block.Outputs),
		CommandDigest:           commandDigest,
		EnvDigest:               envDigest,
		InputManifestDigest:     strings.TrimSpace(inputDigest),
		RepositorySourceDigest:  repositorySourceDigest,
		NormalizedOutputsDigest: outputsDigest,
	}, nil
}

type blockVolumeRepositorySourceIdentity struct {
	CanonicalRemoteURL          string `json:"canonical_remote_url,omitempty"`
	CommitSHA                   string `json:"commit_sha,omitempty"`
	SubmoduleMode               string `json:"submodule_mode,omitempty"`
	ChangesetDigest             string `json:"changeset_digest,omitempty"`
	CheckoutMode                string `json:"checkout_mode,omitempty"`
	MaterializationRecipeDigest string `json:"materialization_recipe_digest,omitempty"`
}

func blockVolumeRepositorySourceDigest(repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset) (string, error) {
	normalizedRepository := normalizeRepositoryCheckoutForComparison(repository)
	if normalizedRepository == nil {
		return "", nil
	}
	return digestCanonicalJSON(blockVolumeRepositorySourceIdentity{
		CanonicalRemoteURL:          strings.TrimSpace(normalizedRepository.RemoteURL),
		CommitSHA:                   strings.TrimSpace(normalizedRepository.CommitSHA),
		SubmoduleMode:               workspaceStageSubmoduleMode(normalizedRepository),
		ChangesetDigest:             strings.TrimSpace(changesetDigest(changeset)),
		CheckoutMode:                workspaceStageCheckoutMode(normalizedRepository),
		MaterializationRecipeDigest: workspaceStageMaterializationRecipeDigest(normalizedRepository),
	})
}

func lookupBlockVolumeCache(ctx context.Context, store cacheMetadataStore, stageName, backendName string, compiled *policy.CompiledPolicy, block blockVolumeBlockPlan) (blockVolumeBlockPlan, error) {
	record, ok, err := store.GetReady(ctx, stageName, block.CacheKey)
	if err != nil {
		return block, err
	}
	if !ok {
		block.LookupReason = observability.CacheLookupReasonRecordNotFound
		block.CacheOutputMinimumBytes = suggestedBlockVolumeMinimumBytes(ctx, store, stageName, backendName, compiled, block)
		return block, nil
	}
	if reason := blockVolumeRecordMissReason(record, backendName, compiled, block); reason != "" {
		block.LookupReason = reason
		block.CacheOutputMinimumBytes = suggestedBlockVolumeMinimumBytes(ctx, store, stageName, backendName, compiled, block)
		return block, nil
	}
	block.CacheHit = true
	block.LookupReason = ""
	block.CacheRecord = record
	return block, nil
}

func suggestedBlockVolumeMinimumBytes(ctx context.Context, store cacheMetadataStore, stageName, backendName string, compiled *policy.CompiledPolicy, block blockVolumeBlockPlan) int64 {
	if store == nil || compiled == nil {
		return 0
	}
	records, err := store.List(ctx)
	if err != nil {
		return 0
	}
	var maxSize int64
	for _, record := range records {
		if !blockVolumeRecordCanInformMinimum(record, stageName, backendName, compiled, block) {
			continue
		}
		if size := blockVolumeRecordMinimumBasisBytes(record); size > maxSize {
			maxSize = size
		}
	}
	return blockVolumeMinimumBytesWithHeadroom(maxSize)
}

func blockVolumeRecordMinimumBasisBytes(record cachestore.Record) int64 {
	if record.ExclusiveSizeBytes > 0 {
		return record.ExclusiveSizeBytes
	}
	return record.StorageSizeBytes
}

func blockVolumeRecordCanInformMinimum(record cachestore.Record, stageName, backendName string, compiled *policy.CompiledPolicy, block blockVolumeBlockPlan) bool {
	if strings.TrimSpace(record.State) != cacheStateReady ||
		strings.TrimSpace(record.Stage) != strings.TrimSpace(stageName) ||
		strings.TrimSpace(record.Backend) != strings.TrimSpace(backendName) ||
		strings.TrimSpace(record.PolicyHash) != strings.TrimSpace(compiled.Hash) ||
		strings.TrimSpace(record.CommandDigest) != strings.TrimSpace(block.CommandDigest) ||
		strings.TrimSpace(record.EnvDigest) != strings.TrimSpace(block.EnvDigest) ||
		strings.TrimSpace(record.NormalizedOutputsDigest) != strings.TrimSpace(block.NormalizedOutputsDigest) ||
		strings.TrimSpace(record.ProducerVersion) != strings.TrimSpace(block.ProducerVersion) ||
		blockVolumeRecordMinimumBasisBytes(record) <= 0 {
		return false
	}
	return blockVolumeOutputRecordMissReason(block.Outputs, record.OutputRecords) == ""
}

func blockVolumeMinimumBytesWithHeadroom(sizeBytes int64) int64 {
	if sizeBytes <= 0 {
		return 0
	}
	headroom := sizeBytes / blockVolumeAdaptiveHeadroomDivisor
	if headroom < blockVolumeAdaptiveHeadroomBytes {
		headroom = blockVolumeAdaptiveHeadroomBytes
	}
	if sizeBytes > blockVolumeMaxInt64-headroom {
		return blockVolumeMaxInt64
	}
	return sizeBytes + headroom
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

func setBlockVolumeLookupSpanAttributes(ctx context.Context, blockCount, hits, misses int, err error) {
	result := observability.CacheResultFailed
	if err == nil {
		result = observability.CacheResultMiss
		if blockCount > 0 && misses == 0 {
			result = observability.CacheResultHit
		}
	}
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String(observability.AttrCacheResult, result),
		attribute.Int("cleanroom.cache.hit_count", hits),
		attribute.Int("cleanroom.cache.miss_count", misses),
	)
}

func addBlockVolumeLookupSpanEvents(ctx context.Context, stage string, blocks []blockVolumeLookupSpanEventBlock, err error) {
	if err != nil || len(blocks) == 0 {
		return
	}
	span := trace.SpanFromContext(ctx)
	for _, block := range blocks {
		attrs := []attribute.KeyValue{
			attribute.String(observability.AttrCacheStage, strings.TrimSpace(stage)),
			attribute.String(observability.AttrCacheBlock, strings.TrimSpace(block.BlockName)),
			attribute.String(observability.AttrCacheKey, strings.TrimSpace(block.CacheKey)),
			attribute.String(observability.AttrCacheResult, blockVolumeLookupResult(block)),
		}
		if reason := strings.TrimSpace(block.LookupReason); reason != "" {
			attrs = append(attrs, attribute.String(observability.AttrCacheLookupReason, reason))
		}
		if dirs := trimmedStringSlice(block.Outputs.Dirs); len(dirs) > 0 {
			attrs = append(attrs, attribute.StringSlice(observability.AttrCacheOutputDirs, dirs))
		}
		if files := trimmedStringSlice(block.Outputs.Files); len(files) > 0 {
			attrs = append(attrs, attribute.StringSlice(observability.AttrCacheOutputFiles, files))
		}
		span.AddEvent("cleanroom.cache.block_lookup", trace.WithAttributes(attrs...))
	}
}

func blockVolumeLookupResult(block blockVolumeLookupSpanEventBlock) string {
	if block.CacheHit {
		return observability.CacheResultHit
	}
	return observability.CacheResultMiss
}

func trimmedStringSlice(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func blockVolumeRecordMissReason(record cachestore.Record, backendName string, compiled *policy.CompiledPolicy, block blockVolumeBlockPlan) string {
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
		strings.TrimSpace(record.ProducerVersion) != strings.TrimSpace(block.ProducerVersion) {
		return observability.CacheLookupReasonRecordNotFound
	}
	if reason := blockVolumeOutputRecordMissReason(block.Outputs, record.OutputRecords); reason != "" {
		return reason
	}
	return ""
}

func blockVolumeOutputRecordMissReason(outputs policy.StageBlockOutputs, records []cachestore.OutputRecord) string {
	expected := make(map[string]struct{}, len(outputs.Dirs)+len(outputs.Files))
	for _, dir := range outputs.Dirs {
		expected[blockVolumeOutputRecordKey("dir", dir)] = struct{}{}
	}
	for _, file := range outputs.Files {
		expected[blockVolumeOutputRecordKey("file", file)] = struct{}{}
	}
	if len(expected) == 0 || len(records) != len(expected) {
		return observability.CacheLookupReasonRecordNotFound
	}

	seen := make(map[string]struct{}, len(records))
	storageDriver := ""
	storageRef := ""
	sourceSnapshotRef := ""
	for _, record := range records {
		kind := strings.TrimSpace(record.Kind)
		path := strings.TrimSpace(record.Path)
		key := blockVolumeOutputRecordKey(kind, path)
		if _, ok := expected[key]; !ok {
			return observability.CacheLookupReasonRecordNotFound
		}
		if _, ok := seen[key]; ok {
			return observability.CacheLookupReasonRecordNotFound
		}
		seen[key] = struct{}{}
		if strings.TrimSpace(record.VolumeSubpath) == "" ||
			strings.TrimSpace(record.StorageDriver) == "" ||
			strings.TrimSpace(record.StorageRef) == "" {
			return observability.CacheLookupReasonRecordNotFound
		}
		recordStorageDriver := strings.TrimSpace(record.StorageDriver)
		recordStorageRef := strings.TrimSpace(record.StorageRef)
		recordSourceSnapshotRef := blockVolumeSourceSnapshotRef(record)
		if storageDriver == "" && storageRef == "" && sourceSnapshotRef == "" {
			storageDriver = recordStorageDriver
			storageRef = recordStorageRef
			sourceSnapshotRef = recordSourceSnapshotRef
			continue
		}
		if recordStorageDriver != storageDriver || recordStorageRef != storageRef || recordSourceSnapshotRef != sourceSnapshotRef {
			return observability.CacheLookupReasonRecordNotFound
		}
	}
	return ""
}

func blockVolumeOutputRecordKey(kind, path string) string {
	return strings.TrimSpace(kind) + "\x00" + strings.TrimSpace(path)
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

func cloneBlockEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for key, value := range env {
		out[key] = value
	}
	return out
}

func cloneDependencyBlockEnv(env map[string]string) map[string]string {
	return cloneBlockEnv(env)
}

func digestCanonicalJSON(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
