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
	NormalizedOutputsDigest         string
	DependencyOutputKeysDigest      string
	PriorDependencyOutputKeysDigest string
	PriorServiceOutputKeysDigest    string
	OutputVolumeLayoutVersion       string
	ProducerVersion                 string
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
	Blocks               []serviceBlockVolumeBlockPlan
}

type serviceBlockVolumeBlockPlan blockVolumeBlockPlan

type blockVolumeRuntimeDecision struct {
	Enabled        bool
	FallbackReason string
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
		NormalizedOutputsDigest: outputsDigest,
	}, nil
}

func lookupBlockVolumeCache(ctx context.Context, store cacheMetadataStore, stageName, backendName string, compiled *policy.CompiledPolicy, block blockVolumeBlockPlan) (blockVolumeBlockPlan, error) {
	record, ok, err := store.GetReady(ctx, stageName, block.CacheKey)
	if err != nil {
		return block, err
	}
	if !ok {
		block.LookupReason = observability.CacheLookupReasonRecordNotFound
		return block, nil
	}
	if reason := blockVolumeRecordMissReason(record, backendName, compiled, block); reason != "" {
		block.LookupReason = reason
		return block, nil
	}
	block.CacheHit = true
	block.LookupReason = ""
	block.CacheRecord = record
	return block, nil
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
