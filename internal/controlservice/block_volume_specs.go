package controlservice

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachestore"
	"github.com/buildkite/cleanroom/internal/policy"
)

type blockVolumeOutputSpecBlock struct {
	Stage       string
	BlockName   string
	CacheKey    string
	Outputs     policy.StageBlockOutputs
	CacheHit    bool
	CacheRecord cachestore.Record
}

func dependencyBlockVolumeOutputSpecs(plan dependencyBlockVolumePlan) ([]backend.CacheOutputVolumeSpec, error) {
	blocks := make([]blockVolumeOutputSpecBlock, 0, len(plan.Blocks))
	for _, block := range plan.Blocks {
		blocks = append(blocks, blockVolumeOutputSpecBlock{
			Stage:       dependencyVolumeStageName,
			BlockName:   block.BlockName,
			CacheKey:    block.CacheKey,
			Outputs:     block.Outputs,
			CacheHit:    block.CacheHit,
			CacheRecord: block.CacheRecord,
		})
	}
	return blockVolumeOutputSpecs(blocks)
}

func serviceBlockVolumeOutputSpecs(plan serviceBlockVolumePlan) ([]backend.CacheOutputVolumeSpec, error) {
	blocks := make([]blockVolumeOutputSpecBlock, 0, len(plan.Blocks))
	for _, block := range plan.Blocks {
		blocks = append(blocks, blockVolumeOutputSpecBlock{
			Stage:       serviceVolumeStageName,
			BlockName:   block.BlockName,
			CacheKey:    block.CacheKey,
			Outputs:     block.Outputs,
			CacheHit:    block.CacheHit,
			CacheRecord: block.CacheRecord,
		})
	}
	return blockVolumeOutputSpecs(blocks)
}

func blockVolumeFileCaptures(stage, cacheKey string, outputs policy.StageBlockOutputs) []backend.CacheOutputFileCapture {
	if len(outputs.Files) == 0 {
		return nil
	}
	volumeID := blockVolumeID(stage, cacheKey)
	captures := make([]backend.CacheOutputFileCapture, 0, len(outputs.Files))
	for i, file := range outputs.Files {
		captures = append(captures, backend.CacheOutputFileCapture{
			VolumeID:      volumeID,
			GuestPath:     strings.TrimSpace(file),
			VolumeSubpath: blockVolumeOutputSubpath("files", i),
		})
	}
	return captures
}

func blockVolumeOutputSpecs(blocks []blockVolumeOutputSpecBlock) ([]backend.CacheOutputVolumeSpec, error) {
	if len(blocks) == 0 {
		return nil, nil
	}
	specs := make([]backend.CacheOutputVolumeSpec, 0, len(blocks))
	for _, block := range blocks {
		spec, err := blockVolumeOutputSpec(block)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func blockVolumeOutputSpec(block blockVolumeOutputSpecBlock) (backend.CacheOutputVolumeSpec, error) {
	stage := strings.TrimSpace(block.Stage)
	blockName := strings.TrimSpace(block.BlockName)
	cacheKey := strings.TrimSpace(block.CacheKey)
	if stage == "" {
		return backend.CacheOutputVolumeSpec{}, fmt.Errorf("block output volume spec missing stage")
	}
	if blockName == "" {
		return backend.CacheOutputVolumeSpec{}, fmt.Errorf("%s block output volume spec missing block name", stage)
	}
	if cacheKey == "" {
		return backend.CacheOutputVolumeSpec{}, fmt.Errorf("%s block %q output volume spec missing cache key", stage, blockName)
	}

	spec := backend.CacheOutputVolumeSpec{
		Stage:     stage,
		BlockName: blockName,
		CacheKey:  cacheKey,
		VolumeID:  blockVolumeID(stage, cacheKey),
	}
	if block.CacheHit {
		outputRecords, err := blockVolumeOutputRecordsByKey(block)
		if err != nil {
			return backend.CacheOutputVolumeSpec{}, err
		}
		for i, dir := range block.Outputs.Dirs {
			record, ok := outputRecords[blockVolumeOutputRecordKey("dir", dir)]
			if !ok {
				return backend.CacheOutputVolumeSpec{}, fmt.Errorf("%s block %q cache hit missing dir output record %q", stage, blockName, dir)
			}
			if i == 0 {
				spec.StorageDriver = strings.TrimSpace(record.StorageDriver)
				spec.StorageRef = strings.TrimSpace(record.StorageRef)
				spec.SourceSnapshotRef = blockVolumeSourceSnapshotRef(record)
			}
			spec.DirMappings = append(spec.DirMappings, backend.CacheOutputDirMapping{
				GuestPath: strings.TrimSpace(dir),
				Subpath:   strings.TrimSpace(record.VolumeSubpath),
			})
		}
		for i, file := range block.Outputs.Files {
			record, ok := outputRecords[blockVolumeOutputRecordKey("file", file)]
			if !ok {
				return backend.CacheOutputVolumeSpec{}, fmt.Errorf("%s block %q cache hit missing file output record %q", stage, blockName, file)
			}
			if len(block.Outputs.Dirs) == 0 && i == 0 {
				spec.StorageDriver = strings.TrimSpace(record.StorageDriver)
				spec.StorageRef = strings.TrimSpace(record.StorageRef)
				spec.SourceSnapshotRef = blockVolumeSourceSnapshotRef(record)
			}
			spec.FileMappings = append(spec.FileMappings, backend.CacheOutputFileMapping{
				GuestPath: strings.TrimSpace(file),
				Subpath:   strings.TrimSpace(record.VolumeSubpath),
			})
		}
		return spec, nil
	}

	for i, dir := range block.Outputs.Dirs {
		spec.DirMappings = append(spec.DirMappings, backend.CacheOutputDirMapping{
			GuestPath: strings.TrimSpace(dir),
			Subpath:   blockVolumeOutputSubpath("dirs", i),
		})
	}
	for i, file := range block.Outputs.Files {
		spec.FileMappings = append(spec.FileMappings, backend.CacheOutputFileMapping{
			GuestPath: strings.TrimSpace(file),
			Subpath:   blockVolumeOutputSubpath("files", i),
		})
	}
	return spec, nil
}

func blockVolumeOutputRecordsByKey(block blockVolumeOutputSpecBlock) (map[string]cachestore.OutputRecord, error) {
	stage := strings.TrimSpace(block.Stage)
	blockName := strings.TrimSpace(block.BlockName)
	records := block.CacheRecord.OutputRecords
	if reason := blockVolumeOutputRecordMissReason(block.Outputs, records); reason != "" {
		return nil, fmt.Errorf("%s block %q cache hit has invalid output records: %s", stage, blockName, reason)
	}
	out := make(map[string]cachestore.OutputRecord, len(records))
	var storageDriver, storageRef, sourceSnapshotRef string
	for _, record := range records {
		key := blockVolumeOutputRecordKey(record.Kind, record.Path)
		if _, ok := out[key]; ok {
			return nil, fmt.Errorf("%s block %q cache hit has duplicate output record %q", stage, blockName, strings.TrimSpace(record.Path))
		}
		recordStorageDriver := strings.TrimSpace(record.StorageDriver)
		recordStorageRef := strings.TrimSpace(record.StorageRef)
		recordSourceSnapshotRef := blockVolumeSourceSnapshotRef(record)
		if storageDriver == "" && storageRef == "" && sourceSnapshotRef == "" {
			storageDriver = recordStorageDriver
			storageRef = recordStorageRef
			sourceSnapshotRef = recordSourceSnapshotRef
		} else if recordStorageDriver != storageDriver || recordStorageRef != storageRef || recordSourceSnapshotRef != sourceSnapshotRef {
			return nil, fmt.Errorf("%s block %q cache hit spans multiple output volumes", stage, blockName)
		}
		out[key] = record
	}
	return out, nil
}

func blockVolumeSourceSnapshotRef(record cachestore.OutputRecord) string {
	if snapshotRef := strings.TrimSpace(record.SnapshotRef); snapshotRef != "" {
		return snapshotRef
	}
	return strings.TrimSpace(record.StorageRef)
}

func blockVolumeOutputSubpath(kind string, index int) string {
	return fmt.Sprintf("%s/%d", kind, index)
}

func blockVolumeID(stage, cacheKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(stage) + "\x00" + strings.TrimSpace(cacheKey)))
	return strings.TrimSpace(stage) + "-" + hex.EncodeToString(sum[:12])
}

func appendCacheOutputVolumeSpecs(groups ...[]backend.CacheOutputVolumeSpec) []backend.CacheOutputVolumeSpec {
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	if total == 0 {
		return nil
	}
	out := make([]backend.CacheOutputVolumeSpec, 0, total)
	for _, group := range groups {
		out = append(out, cloneCacheOutputVolumeSpecs(group)...)
	}
	return out
}

func cloneCacheOutputVolumeSpecs(specs []backend.CacheOutputVolumeSpec) []backend.CacheOutputVolumeSpec {
	if len(specs) == 0 {
		return nil
	}
	out := make([]backend.CacheOutputVolumeSpec, len(specs))
	for i, spec := range specs {
		out[i] = spec
		out[i].DirMappings = append([]backend.CacheOutputDirMapping(nil), spec.DirMappings...)
		out[i].FileMappings = append([]backend.CacheOutputFileMapping(nil), spec.FileMappings...)
	}
	return out
}
