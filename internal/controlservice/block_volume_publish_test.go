package controlservice

import (
	"context"
	"slices"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

func TestBlockVolumePublishCleansAllSnapshotsWhenOutputRecordValidationFails(t *testing.T) {
	adapter := dependencyBlockVolumeRuntimeAdapter()
	var deletedSnapshotIDs []string
	adapter.deleteSnapshotFn = func(_ context.Context, req backend.DeleteSnapshotRequest) error {
		deletedSnapshotIDs = append(deletedSnapshotIDs, req.SnapshotID)
		return nil
	}

	svc := newTestService(adapter)
	compiled, err := policy.FromProto(testRepositoryPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      "0123456789abcdef0123456789abcdef01234567",
		DestinationDir: "/workspace",
	}
	blocks := []blockVolumePublishBlock{
		{
			BlockName:               "one",
			CacheKey:                "cache-one",
			Outputs:                 policy.StageBlockOutputs{Dirs: []string{"/root/out-one"}},
			CommandDigest:           "sha256:command-one",
			EnvDigest:               "sha256:env-one",
			InputManifestDigest:     "sha256:inputs-one",
			NormalizedOutputsDigest: "sha256:outputs-one",
			ProducerVersion:         dependencyVolumeProducerVersion,
		},
		{
			BlockName:               "two",
			CacheKey:                "cache-two",
			Outputs:                 policy.StageBlockOutputs{Dirs: []string{"/root/out-two"}},
			CommandDigest:           "sha256:command-two",
			EnvDigest:               "sha256:env-two",
			InputManifestDigest:     "sha256:inputs-two",
			NormalizedOutputsDigest: "sha256:outputs-two",
			ProducerVersion:         dependencyVolumeProducerVersion,
		},
	}
	adapter.snapshotCacheOutputsFn = func(_ context.Context, req backend.SnapshotCacheOutputVolumesRequest) (*backend.SnapshotCacheOutputVolumesResult, error) {
		return &backend.SnapshotCacheOutputVolumesResult{Volumes: []backend.CacheOutputVolumeSnapshot{
			{
				VolumeID:      req.VolumeIDs[0],
				SnapshotID:    "snapshot-one",
				StorageDriver: "file",
				StorageRef:    "/snapshots/one.ext4",
				Outputs: []backend.CacheOutputVolumeSnapshotOutput{
					{Kind: "dir", GuestPath: "/root/unexpected", VolumeSubpath: "dirs/0"},
				},
			},
			{
				VolumeID:      req.VolumeIDs[1],
				SnapshotID:    "snapshot-two",
				StorageDriver: "file",
				StorageRef:    "/snapshots/two.ext4",
				Outputs: []backend.CacheOutputVolumeSnapshotOutput{
					{Kind: "dir", GuestPath: "/root/out-two", VolumeSubpath: "dirs/0"},
				},
			},
		}}, nil
	}

	svc.maybePublishBlockVolumeCaches(context.Background(), adapter, "sandbox-1", "firecracker", compiled, backend.FirecrackerConfig{}, repository, nil, dependencyBlockVolumePublishPhase, blocks)

	slices.Sort(deletedSnapshotIDs)
	if got, want := deletedSnapshotIDs, []string{"snapshot-one", "snapshot-two"}; !slices.Equal(got, want) {
		t.Fatalf("unexpected deleted snapshots: got %v want %v", got, want)
	}
	cacheStore := svc.CacheStore.(*memoryCacheStore)
	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected no cache records after validation failure, got %d", len(records))
	}
}
