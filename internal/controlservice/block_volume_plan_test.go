package controlservice

import (
	"context"
	"testing"

	"github.com/buildkite/cleanroom/internal/cachestore"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
)

func TestLookupBlockVolumeCacheSuggestsMinimumFromPriorMatchingRecord(t *testing.T) {
	ctx := context.Background()
	store := newMemoryCacheStore()
	compiled := &policy.CompiledPolicy{Hash: "policy-hash"}
	block := blockVolumeBlockPlan{
		BlockName:               "docker-images",
		CacheKey:                "service-volume:new-input",
		CommandDigest:           "sha256:command",
		EnvDigest:               "sha256:env",
		NormalizedOutputsDigest: "sha256:outputs",
		ProducerVersion:         serviceVolumeProducerVersion,
		Outputs: policy.StageBlockOutputs{
			Dirs: []string{"/var/lib/docker"},
		},
	}
	if err := store.Create(ctx, cachestore.Record{
		CacheKey:                "service-volume:old-input",
		Stage:                   serviceVolumeStageName,
		State:                   cacheStateReady,
		Backend:                 "darwin-vz",
		PolicyHash:              compiled.Hash,
		CommandDigest:           block.CommandDigest,
		EnvDigest:               block.EnvDigest,
		NormalizedOutputsDigest: block.NormalizedOutputsDigest,
		ProducerVersion:         block.ProducerVersion,
		StorageSizeBytes:        32 << 30,
		ExclusiveSizeBytes:      20 << 30,
		OutputRecords: []cachestore.OutputRecord{{
			Kind:          "dir",
			Path:          "/var/lib/docker",
			VolumeSubpath: "dirs/0",
			StorageDriver: "apfs",
			StorageRef:    "/snapshots/docker.ext4",
			SnapshotRef:   "/snapshots/docker.ext4",
		}},
	}); err != nil {
		t.Fatalf("create prior cache record: %v", err)
	}
	if err := store.Create(ctx, cachestore.Record{
		CacheKey:                "service-volume:wrong-output",
		Stage:                   serviceVolumeStageName,
		State:                   cacheStateReady,
		Backend:                 "darwin-vz",
		PolicyHash:              compiled.Hash,
		CommandDigest:           block.CommandDigest,
		EnvDigest:               block.EnvDigest,
		NormalizedOutputsDigest: block.NormalizedOutputsDigest,
		ProducerVersion:         block.ProducerVersion,
		StorageSizeBytes:        64 << 30,
		OutputRecords: []cachestore.OutputRecord{{
			Kind:          "dir",
			Path:          "/var/lib/postgresql",
			VolumeSubpath: "dirs/0",
			StorageDriver: "apfs",
			StorageRef:    "/snapshots/postgres.ext4",
			SnapshotRef:   "/snapshots/postgres.ext4",
		}},
	}); err != nil {
		t.Fatalf("create nonmatching cache record: %v", err)
	}

	got, err := lookupBlockVolumeCache(ctx, store, serviceVolumeStageName, "darwin-vz", compiled, block)
	if err != nil {
		t.Fatalf("lookupBlockVolumeCache returned error: %v", err)
	}
	if got.CacheHit {
		t.Fatal("expected cache miss for new input")
	}
	if got.LookupReason != observability.CacheLookupReasonRecordNotFound {
		t.Fatalf("unexpected lookup reason: got %q", got.LookupReason)
	}
	if got, want := got.CacheOutputMinimumBytes, int64(25<<30); got != want {
		t.Fatalf("unexpected adaptive minimum bytes: got %d want %d", got, want)
	}
}

func TestBlockVolumeMinimumBytesWithHeadroomUsesAtLeastOneGiB(t *testing.T) {
	if got, want := blockVolumeMinimumBytesWithHeadroom(2<<30), int64(3<<30); got != want {
		t.Fatalf("unexpected minimum bytes with headroom: got %d want %d", got, want)
	}
}
