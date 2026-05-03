package controlservice

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

func TestMaybePublishDependencyBlockVolumeCachesPublishesMisses(t *testing.T) {
	adapter := dependencyBlockVolumeRuntimeAdapter()
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml": "go = \"1.26.2\"\n",
		"go.mod":    "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":    "example.com/test v0.0.0 h1:abc123\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	svc.runtime.clock = stubClock{now: time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)}

	compiled, err := policy.FromProto(testRepositoryTwoDependencyBlocksPolicy())
	if err != nil {
		t.Fatalf("policy.FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	plan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected dependency block-volume plan")
	}

	svc.maybePublishDependencyBlockVolumeCaches(
		context.Background(),
		adapter,
		adapter,
		"cr-test",
		"firecracker",
		compiled,
		backend.FirecrackerConfig{},
		repository,
		nil,
		plan,
		nil,
	)

	if got, want := adapter.snapshotCacheOutputsCalls, 1; got != want {
		t.Fatalf("unexpected snapshot call count: got %d want %d", got, want)
	}
	if got, want := len(adapter.snapshotCacheOutputsReq.Volumes), len(plan.Blocks); got != want {
		t.Fatalf("unexpected snapshot volume count: got %d want %d", got, want)
	}
	if !adapter.snapshotCacheOutputsReq.FirecrackerConfig.Snapshots.Enabled {
		t.Fatal("expected snapshot config to be enabled")
	}
	if got, want := adapter.snapshotCacheOutputsReq.FirecrackerConfig.Snapshots.Driver, "file"; got != want {
		t.Fatalf("unexpected snapshot driver: got %q want %q", got, want)
	}

	for i, block := range plan.Blocks {
		volumeReq := adapter.snapshotCacheOutputsReq.Volumes[i]
		if got, want := volumeReq.VolumeID, blockVolumeID(dependencyVolumeStageName, block.CacheKey); got != want {
			t.Fatalf("unexpected volume id for block %q: got %q want %q", block.BlockName, got, want)
		}
		if got, want := volumeReq.BlockName, block.BlockName; got != want {
			t.Fatalf("unexpected snapshot block name: got %q want %q", got, want)
		}

		record, found, err := svc.CacheStore.GetReady(context.Background(), dependencyVolumeStageName, block.CacheKey)
		if err != nil {
			t.Fatalf("GetReady returned error for block %q: %v", block.BlockName, err)
		}
		if !found {
			t.Fatalf("expected published cache record for block %q", block.BlockName)
		}
		if got, want := record.ReuseMode, dependencyVolumeReuseMode; got != want {
			t.Fatalf("unexpected reuse mode for block %q: got %q want %q", block.BlockName, got, want)
		}
		if got, want := record.Backend, "firecracker"; got != want {
			t.Fatalf("unexpected backend for block %q: got %q want %q", block.BlockName, got, want)
		}
		if got, want := record.RuntimeBaseKey, "runtime-base:test"; got != want {
			t.Fatalf("unexpected runtime base key for block %q: got %q want %q", block.BlockName, got, want)
		}
		if got, want := record.StorageDriver, "file"; got != want {
			t.Fatalf("unexpected storage driver for block %q: got %q want %q", block.BlockName, got, want)
		}
		if got, want := record.StorageRef, "/tmp/"+volumeReq.VolumeID+".ext4"; got != want {
			t.Fatalf("unexpected storage ref for block %q: got %q want %q", block.BlockName, got, want)
		}
		if got, want := record.BackingSnapshotID, volumeReq.SnapshotID; got != want {
			t.Fatalf("unexpected snapshot id for block %q: got %q want %q", block.BlockName, got, want)
		}
		if got, want := record.ParentCacheKey, block.PriorDependencyOutputKeysDigest; got != want {
			t.Fatalf("unexpected parent cache key for block %q: got %q want %q", block.BlockName, got, want)
		}
		if !strings.HasPrefix(record.OutputManifestDigest, "sha256:") {
			t.Fatalf("expected output manifest digest for block %q, got %q", block.BlockName, record.OutputManifestDigest)
		}
		if got, want := len(record.OutputRecords), len(block.Outputs.Dirs); got != want {
			t.Fatalf("unexpected output record count for block %q: got %d want %d", block.BlockName, got, want)
		}
		if got, want := record.OutputRecords[0].Path, block.Outputs.Dirs[0]; got != want {
			t.Fatalf("unexpected output record path for block %q: got %q want %q", block.BlockName, got, want)
		}
		if got, want := record.OutputRecords[0].VolumeSubpath, "dirs/0"; got != want {
			t.Fatalf("unexpected output record subpath for block %q: got %q want %q", block.BlockName, got, want)
		}
	}

	lookedUp, err := svc.lookupDependencyBlockVolumeCaches(context.Background(), "firecracker", compiled, plan)
	if err != nil {
		t.Fatalf("lookupDependencyBlockVolumeCaches returned error: %v", err)
	}
	for _, block := range lookedUp.Blocks {
		if !block.CacheHit {
			t.Fatalf("expected published block %q to be a cache hit, reason=%q", block.BlockName, block.LookupReason)
		}
	}
}

func TestMaybePublishDependencyBlockVolumeCachesSkipsFileOutputs(t *testing.T) {
	adapter := dependencyBlockVolumeRuntimeAdapter()
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"Gemfile.lock": "GEM\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	policyProto := testRepositoryPolicy()
	policyProto.Dependencies = &cleanroomv1.PolicyDependencies{
		Blocks: []*cleanroomv1.PolicyBlock{testPolicyBlock(
			"bundler",
			[]string{"bundle", "install"},
			[]string{"Gemfile.lock"},
			nil,
			[]string{"/root/.bundle/config"},
		)},
	}
	compiled, err := policy.FromProto(policyProto)
	if err != nil {
		t.Fatalf("policy.FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	plan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected dependency block-volume plan")
	}

	svc.maybePublishDependencyBlockVolumeCaches(
		context.Background(),
		adapter,
		adapter,
		"cr-test",
		"firecracker",
		compiled,
		backend.FirecrackerConfig{},
		repository,
		nil,
		plan,
		nil,
	)

	if got := adapter.snapshotCacheOutputsCalls; got != 0 {
		t.Fatalf("expected file-output block to skip snapshot publication, got %d snapshot calls", got)
	}
	if _, found, err := svc.CacheStore.GetReady(context.Background(), dependencyVolumeStageName, plan.Blocks[0].CacheKey); err != nil {
		t.Fatalf("GetReady returned error: %v", err)
	} else if found {
		t.Fatal("did not expect cache record for file-output block")
	}
}
