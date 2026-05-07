package controlservice

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachestore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

func TestFinalizeServiceBlockVolumePlanBuildsOrderedKeys(t *testing.T) {
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml":           "go = \"1.26.2\"\n",
		"go.mod":              "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":              "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml":  "services:\n  postgres:\n    image: postgres:17\n",
		"db/schema.sql":       "create table widgets (id serial primary key);\n",
		"db/seed.sql":         "insert into widgets default values;\n",
		"scripts/prepare-db":  "#!/bin/sh\ntrue\n",
		"scripts/prepare-app": "#!/bin/sh\ntrue\n",
	})
	svc := newTestService(&stubAdapter{})
	svc.RepositoryStore = mirrors

	compiled, err := policy.FromProto(testRepositoryTwoDependencyTwoServiceBlocksPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	dependencyPlan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected dependency block-volume plan")
	}

	plan, ok, err := svc.finalizeServiceBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test", dependencyPlan)
	if err != nil {
		t.Fatalf("finalizeServiceBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected service block-volume plan")
	}
	if got, want := plan.ReuseNamespace, "https://github.com/buildkite/cleanroom.git"; got != want {
		t.Fatalf("unexpected reuse namespace: got %q want %q", got, want)
	}
	if got, want := len(plan.Blocks), 2; got != want {
		t.Fatalf("unexpected service block count: got %d want %d", got, want)
	}
	if got, want := len(plan.DependencyOutputKeys), len(dependencyPlan.Blocks); got != want {
		t.Fatalf("unexpected dependency output key count: got %d want %d", got, want)
	}

	first := plan.Blocks[0]
	second := plan.Blocks[1]
	if first.CacheKey == "" || second.CacheKey == "" {
		t.Fatalf("expected non-empty block cache keys: first=%q second=%q", first.CacheKey, second.CacheKey)
	}
	if first.CacheKey == second.CacheKey {
		t.Fatalf("expected distinct service block cache keys, got %q", first.CacheKey)
	}
	dependencyKeysDigest, err := digestCanonicalJSON(dependencyBlockVolumePlanCacheKeys(dependencyPlan))
	if err != nil {
		t.Fatalf("digest dependency output keys: %v", err)
	}
	if got := first.DependencyOutputKeysDigest; got != dependencyKeysDigest {
		t.Fatalf("unexpected dependency output digest: got %q want %q", got, dependencyKeysDigest)
	}
	emptyPriorDigest, err := digestCanonicalJSON([]string{})
	if err != nil {
		t.Fatalf("digest empty prior keys: %v", err)
	}
	if got := first.PriorServiceOutputKeysDigest; got != emptyPriorDigest {
		t.Fatalf("unexpected first prior output digest: got %q want %q", got, emptyPriorDigest)
	}
	firstKeyDigest, err := digestCanonicalJSON([]string{first.CacheKey})
	if err != nil {
		t.Fatalf("digest first cache key: %v", err)
	}
	if got := second.PriorServiceOutputKeysDigest; got != firstKeyDigest {
		t.Fatalf("unexpected second prior output digest: got %q want %q", got, firstKeyDigest)
	}
	for _, block := range plan.Blocks {
		for name, value := range map[string]string{
			"command":           block.CommandDigest,
			"env":               block.EnvDigest,
			"inputs":            block.InputManifestDigest,
			"repository_source": block.RepositorySourceDigest,
			"outputs":           block.NormalizedOutputsDigest,
		} {
			if !strings.HasPrefix(value, "sha256:") {
				t.Fatalf("expected %s digest for block %q, got %q", name, block.BlockName, value)
			}
		}
	}

	mutatedDependencyPlan := dependencyPlan
	mutatedDependencyPlan.Blocks = append([]dependencyBlockVolumeBlockPlan(nil), dependencyPlan.Blocks...)
	mutatedDependencyPlan.Blocks[0].CacheKey = "dependency-volume:v1:different"
	mutatedPlan, ok, err := svc.finalizeServiceBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test", mutatedDependencyPlan)
	if err != nil {
		t.Fatalf("finalizeServiceBlockVolumePlan mutated returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected mutated service block-volume plan")
	}
	if got, want := mutatedPlan.Blocks[0].CacheKey, first.CacheKey; got == want {
		t.Fatalf("expected first service block key to change after dependency output key mutation, got %q", got)
	}

	mutatedRepository := *repository
	mutatedRepository.DestinationDir = "/src"
	mutatedPlan, ok, err = svc.finalizeServiceBlockVolumePlan(context.Background(), compiled, &mutatedRepository, nil, nil, "firecracker", "runtime-base:test", dependencyPlan)
	if err != nil {
		t.Fatalf("finalizeServiceBlockVolumePlan destination mutation returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected destination-mutated service block-volume plan")
	}
	if got, want := mutatedPlan.Blocks[0].CacheKey, first.CacheKey; got == want {
		t.Fatalf("expected first service block key to change after destination dir mutation, got %q", got)
	}

	mutatedRepository = *repository
	mutatedRepository.Branch = "main"
	mutatedPlan, ok, err = svc.finalizeServiceBlockVolumePlan(context.Background(), compiled, &mutatedRepository, nil, nil, "firecracker", "runtime-base:test", dependencyPlan)
	if err != nil {
		t.Fatalf("finalizeServiceBlockVolumePlan repository source mutation returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected repository-source-mutated service block-volume plan")
	}
	if got, want := mutatedPlan.Blocks[0].CacheKey, first.CacheKey; got == want {
		t.Fatalf("expected first service block key to change after repository source mutation, got %q", got)
	}
}

func TestLookupServiceBlockVolumeCachesReportsPartialHit(t *testing.T) {
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml":           "go = \"1.26.2\"\n",
		"go.mod":              "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":              "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml":  "services:\n  postgres:\n    image: postgres:17\n",
		"db/schema.sql":       "create table widgets (id serial primary key);\n",
		"db/seed.sql":         "insert into widgets default values;\n",
		"scripts/prepare-db":  "#!/bin/sh\ntrue\n",
		"scripts/prepare-app": "#!/bin/sh\ntrue\n",
	})
	svc := newTestService(&stubAdapter{})
	svc.RepositoryStore = mirrors

	compiled, err := policy.FromProto(testRepositoryTwoDependencyTwoServiceBlocksPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	dependencyPlan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected dependency block-volume plan")
	}
	plan, ok, err := svc.finalizeServiceBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test", dependencyPlan)
	if err != nil {
		t.Fatalf("finalizeServiceBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected service block-volume plan")
	}

	first := plan.Blocks[0]
	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	if err := cacheStore.Create(context.Background(), serviceBlockVolumeTestRecord(compiled, first)); err != nil {
		t.Fatalf("Create cache record returned error: %v", err)
	}

	lookedUp, err := svc.lookupServiceBlockVolumeCaches(context.Background(), "firecracker", compiled, plan)
	if err != nil {
		t.Fatalf("lookupServiceBlockVolumeCaches returned error: %v", err)
	}
	if got, want := len(lookedUp.Blocks), 2; got != want {
		t.Fatalf("unexpected looked up block count: got %d want %d", got, want)
	}
	if !lookedUp.Blocks[0].CacheHit {
		t.Fatalf("expected first block cache hit, reason=%q", lookedUp.Blocks[0].LookupReason)
	}
	if lookedUp.Blocks[0].CacheRecord.CacheKey != first.CacheKey {
		t.Fatalf("unexpected first block record key: got %q want %q", lookedUp.Blocks[0].CacheRecord.CacheKey, first.CacheKey)
	}
	if lookedUp.Blocks[1].CacheHit {
		t.Fatal("did not expect second block cache hit")
	}
	if got, want := lookedUp.Blocks[1].LookupReason, observability.CacheLookupReasonRecordNotFound; got != want {
		t.Fatalf("unexpected second block miss reason: got %q want %q", got, want)
	}
}

func TestLookupServiceBlockVolumeCachesRejectsMismatchedOutputRecords(t *testing.T) {
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml":           "go = \"1.26.2\"\n",
		"go.mod":              "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":              "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml":  "services:\n  postgres:\n    image: postgres:17\n",
		"db/schema.sql":       "create table widgets (id serial primary key);\n",
		"db/seed.sql":         "insert into widgets default values;\n",
		"scripts/prepare-db":  "#!/bin/sh\ntrue\n",
		"scripts/prepare-app": "#!/bin/sh\ntrue\n",
	})
	svc := newTestService(&stubAdapter{})
	svc.RepositoryStore = mirrors

	compiled, err := policy.FromProto(testRepositoryTwoDependencyTwoServiceBlocksPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	dependencyPlan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected dependency block-volume plan")
	}
	plan, ok, err := svc.finalizeServiceBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test", dependencyPlan)
	if err != nil {
		t.Fatalf("finalizeServiceBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected service block-volume plan")
	}

	record := serviceBlockVolumeTestRecord(compiled, plan.Blocks[0])
	record.OutputRecords[0].Kind = "file"
	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	if err := cacheStore.Create(context.Background(), record); err != nil {
		t.Fatalf("Create cache record returned error: %v", err)
	}

	lookedUp, err := svc.lookupServiceBlockVolumeCaches(context.Background(), "firecracker", compiled, plan)
	if err != nil {
		t.Fatalf("lookupServiceBlockVolumeCaches returned error: %v", err)
	}
	if lookedUp.Blocks[0].CacheHit {
		t.Fatal("did not expect cache hit with mismatched output record kind")
	}
	if got, want := lookedUp.Blocks[0].LookupReason, observability.CacheLookupReasonRecordNotFound; got != want {
		t.Fatalf("unexpected lookup reason: got %q want %q", got, want)
	}
}

func TestCreateSandboxSkipsServiceBlockVolumeLookupWhenBackendUnsupported(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml":           "go = \"1.26.2\"\n",
		"go.mod":              "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":              "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml":  "services:\n  postgres:\n    image: postgres:17\n",
		"db/schema.sql":       "create table widgets (id serial primary key);\n",
		"db/seed.sql":         "insert into widgets default values;\n",
		"scripts/prepare-db":  "#!/bin/sh\ntrue\n",
		"scripts/prepare-app": "#!/bin/sh\ntrue\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	cacheStore := &recordingCacheStore{inner: newMemoryCacheStore()}
	svc.CacheStore = cacheStore

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryTwoDependencyTwoServiceBlocksPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}); err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got := cacheStore.getReadyCount(serviceVolumeStageName); got != 0 {
		t.Fatalf("expected unsupported backend to skip service block-volume cache lookups, got %d", got)
	}
	if got, want := adapter.runCalls, 3; got != want {
		t.Fatalf("expected aggregate repository + dependency + services bootstrap executions, got %d want %d", got, want)
	}
}

func TestCreateSandboxFallsBackWhenServiceBlockVolumeStoreMissing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	adapter := dependencyBlockVolumeRuntimeAdapter()
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml":           "go = \"1.26.2\"\n",
		"go.mod":              "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":              "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml":  "services:\n  postgres:\n    image: postgres:17\n",
		"db/schema.sql":       "create table widgets (id serial primary key);\n",
		"db/seed.sql":         "insert into widgets default values;\n",
		"scripts/prepare-db":  "#!/bin/sh\ntrue\n",
		"scripts/prepare-app": "#!/bin/sh\ntrue\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	svc.CacheStore = nil

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryTwoDependencyTwoServiceBlocksPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}); err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got, want := adapter.runCalls, 3; got != want {
		t.Fatalf("expected aggregate repository + dependency + services bootstrap executions, got %d want %d", got, want)
	}
}

func TestCreateSandboxLooksUpServiceBlockVolumeCaches(t *testing.T) {
	tests := []struct {
		name         string
		records      int
		wantHits     int
		wantRunCalls int
	}{
		{
			name:         "partial hit",
			records:      1,
			wantHits:     1,
			wantRunCalls: 4,
		},
		{
			name:         "all hit",
			records:      2,
			wantHits:     2,
			wantRunCalls: 3,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			adapter := dependencyBlockVolumeRuntimeAdapter()
			mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
				"mise.toml":           "go = \"1.26.2\"\n",
				"go.mod":              "module example.com/test\n\ngo 1.26.2\n",
				"go.sum":              "example.com/test v0.0.0 h1:abc123\n",
				"docker-compose.yml":  "services:\n  postgres:\n    image: postgres:17\n",
				"db/schema.sql":       "create table widgets (id serial primary key);\n",
				"db/seed.sql":         "insert into widgets default values;\n",
				"scripts/prepare-db":  "#!/bin/sh\ntrue\n",
				"scripts/prepare-app": "#!/bin/sh\ntrue\n",
			})
			svc := newTestService(adapter)
			svc.RepositoryStore = mirrors
			cacheStore := &recordingCacheStore{inner: newMemoryCacheStore()}
			svc.CacheStore = cacheStore

			compiled, err := policy.FromProto(testRepositoryTwoDependencyTwoServiceBlocksPolicy())
			if err != nil {
				t.Fatalf("FromProto returned error: %v", err)
			}
			repository := repositorycheckout.FromProto(repositoryCheckout)
			dependencyPlan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
			if err != nil {
				t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
			}
			if !ok {
				t.Fatal("expected dependency block-volume plan")
			}
			plan, ok, err := svc.finalizeServiceBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test", dependencyPlan)
			if err != nil {
				t.Fatalf("finalizeServiceBlockVolumePlan returned error: %v", err)
			}
			if !ok {
				t.Fatal("expected service block-volume plan")
			}
			for i := 0; i < tc.records; i++ {
				if err := cacheStore.Create(context.Background(), serviceBlockVolumeTestRecord(compiled, plan.Blocks[i])); err != nil {
					t.Fatalf("Create service block-volume record %d returned error: %v", i, err)
				}
			}

			if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
				Policy:             testRepositoryTwoDependencyTwoServiceBlocksPolicy(),
				RepositoryCheckout: repositoryCheckout,
			}); err != nil {
				t.Fatalf("CreateSandbox returned error: %v", err)
			}
			if got, want := cacheStore.getReadyCount(serviceVolumeStageName), len(plan.Blocks); got != want {
				t.Fatalf("unexpected service block-volume lookup count: got %d want %d", got, want)
			}
			if got := cacheStore.getReadyHitCount(serviceVolumeStageName); got != tc.wantHits {
				t.Fatalf("unexpected service block-volume hit count: got %d want %d", got, tc.wantHits)
			}
			gotKeys := cacheStore.getReadyKeys(serviceVolumeStageName)
			for i, wantKey := range []string{plan.Blocks[0].CacheKey, plan.Blocks[1].CacheKey} {
				if got := gotKeys[i]; got != wantKey {
					t.Fatalf("unexpected lookup key %d: got %q want %q", i, got, wantKey)
				}
			}
			if got, want := adapter.runCalls, tc.wantRunCalls; got != want {
				t.Fatalf("unexpected bootstrap execution count: got %d want %d", got, want)
			}
		})
	}
}

func TestCreateSandboxPublishesServiceBlockVolumeCachesForMisses(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	adapter := dependencyBlockVolumeRuntimeAdapter()
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml":           "go = \"1.26.2\"\n",
		"go.mod":              "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":              "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml":  "services:\n  postgres:\n    image: postgres:17\n",
		"db/schema.sql":       "create table widgets (id serial primary key);\n",
		"db/seed.sql":         "insert into widgets default values;\n",
		"scripts/prepare-db":  "#!/bin/sh\ntrue\n",
		"scripts/prepare-app": "#!/bin/sh\ntrue\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	cacheStore := newMemoryCacheStore()
	svc.CacheStore = cacheStore

	compiled, err := policy.FromProto(testRepositoryTwoDependencyTwoServiceBlocksPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	dependencyPlan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected dependency block-volume plan")
	}
	for _, block := range dependencyPlan.Blocks {
		if err := cacheStore.Create(context.Background(), dependencyBlockVolumeTestRecord(compiled, block)); err != nil {
			t.Fatalf("Create dependency block-volume record returned error: %v", err)
		}
	}
	servicePlan, ok, err := svc.finalizeServiceBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test", dependencyPlan)
	if err != nil {
		t.Fatalf("finalizeServiceBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected service block-volume plan")
	}
	blockByVolumeID := make(map[string]serviceBlockVolumeBlockPlan, len(servicePlan.Blocks))
	wantVolumeIDs := make([]string, 0, len(servicePlan.Blocks))
	for _, block := range servicePlan.Blocks {
		volumeID := blockVolumeID(serviceVolumeStageName, block.CacheKey)
		blockByVolumeID[volumeID] = block
		wantVolumeIDs = append(wantVolumeIDs, volumeID)
	}
	gotSnapshotVolumeIDs := make([]string, 0, len(servicePlan.Blocks))

	adapter.snapshotCacheOutputsFn = func(_ context.Context, req backend.SnapshotCacheOutputVolumesRequest) (*backend.SnapshotCacheOutputVolumesResult, error) {
		if got, want := req.SandboxID, adapter.provisionReq.SandboxID; got != want {
			t.Fatalf("unexpected snapshot sandbox id: got %q want %q", got, want)
		}
		if strings.TrimSpace(req.SnapshotIDPrefix) == "" {
			t.Fatal("expected snapshot id prefix")
		}
		if got, want := len(req.VolumeIDs), 1; got != want {
			t.Fatalf("unexpected snapshot volume id count: got %d want %d (%v)", got, want, req.VolumeIDs)
		}
		volumeID := req.VolumeIDs[0]
		gotSnapshotVolumeIDs = append(gotSnapshotVolumeIDs, volumeID)
		block, ok := blockByVolumeID[volumeID]
		if !ok {
			t.Fatalf("unexpected snapshot volume id %q", volumeID)
		}
		return &backend.SnapshotCacheOutputVolumesResult{Volumes: []backend.CacheOutputVolumeSnapshot{
			{
				Stage:              serviceVolumeStageName,
				BlockName:          block.BlockName,
				CacheKey:           block.CacheKey,
				VolumeID:           volumeID,
				SnapshotID:         req.SnapshotIDPrefix + "-" + block.BlockName,
				StorageDriver:      "file",
				StorageRef:         "/snapshots/" + block.BlockName + ".ext4",
				SnapshotRef:        "snapshot:" + block.BlockName,
				StorageSizeBytes:   100,
				ExclusiveSizeBytes: 10,
				DriverMetadata:     "metadata:" + block.BlockName,
				Outputs: []backend.CacheOutputVolumeSnapshotOutput{
					{
						Kind:          "dir",
						GuestPath:     block.Outputs.Dirs[0],
						VolumeSubpath: "dirs/0",
					},
				},
			},
		}}, nil
	}

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryTwoDependencyTwoServiceBlocksPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}); err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got, want := adapter.snapshotCacheOutputsCalls, len(servicePlan.Blocks); got != want {
		t.Fatalf("unexpected cache output snapshot calls: got %d want %d", got, want)
	}
	if !slices.Equal(gotSnapshotVolumeIDs, wantVolumeIDs) {
		t.Fatalf("unexpected snapshot volume ids: got %v want %v", gotSnapshotVolumeIDs, wantVolumeIDs)
	}
	if got, want := adapter.runCalls, 1+len(servicePlan.Blocks); got != want {
		t.Fatalf("expected repository plus service block miss executions, got %d want %d", got, want)
	}

	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	recordsByKey := make(map[string]cachestore.Record)
	for _, record := range records {
		if record.Stage == serviceVolumeStageName {
			recordsByKey[record.CacheKey] = record
		}
	}
	if got, want := len(recordsByKey), len(servicePlan.Blocks); got != want {
		t.Fatalf("unexpected service block-volume record count: got %d want %d", got, want)
	}
	for _, block := range servicePlan.Blocks {
		record, ok := recordsByKey[block.CacheKey]
		if !ok {
			t.Fatalf("missing service block-volume record for %q", block.BlockName)
		}
		if got := record.BackingSnapshotID; strings.TrimSpace(got) == "" || !strings.HasSuffix(got, "-"+block.BlockName) {
			t.Fatalf("expected generated backing snapshot id for %s, got %q", block.BlockName, got)
		}
		if got, want := record.Backend, "firecracker"; got != want {
			t.Fatalf("unexpected backend: got %q want %q", got, want)
		}
		if got, want := record.PolicyHash, compiled.Hash; got != want {
			t.Fatalf("unexpected policy hash: got %q want %q", got, want)
		}
		if got, want := record.InputManifestDigest, block.InputManifestDigest; got != want {
			t.Fatalf("unexpected input digest for %s: got %q want %q", block.BlockName, got, want)
		}
		if got, want := record.CommandDigest, block.CommandDigest; got != want {
			t.Fatalf("unexpected command digest for %s: got %q want %q", block.BlockName, got, want)
		}
		if got, want := record.EnvDigest, block.EnvDigest; got != want {
			t.Fatalf("unexpected env digest for %s: got %q want %q", block.BlockName, got, want)
		}
		if got, want := record.NormalizedOutputsDigest, block.NormalizedOutputsDigest; got != want {
			t.Fatalf("unexpected outputs digest for %s: got %q want %q", block.BlockName, got, want)
		}
		if got, want := record.ProducerVersion, block.ProducerVersion; got != want {
			t.Fatalf("unexpected producer version for %s: got %q want %q", block.BlockName, got, want)
		}
		if got, want := record.StorageRef, "/snapshots/"+block.BlockName+".ext4"; got != want {
			t.Fatalf("unexpected storage ref for %s: got %q want %q", block.BlockName, got, want)
		}
		if got, want := record.StorageDriver, "file"; got != want {
			t.Fatalf("unexpected storage driver for %s: got %q want %q", block.BlockName, got, want)
		}
		if got, want := len(record.OutputRecords), 1; got != want {
			t.Fatalf("unexpected output record count for %s: got %d want %d", block.BlockName, got, want)
		}
		output := record.OutputRecords[0]
		if got, want := output.Kind, "dir"; got != want {
			t.Fatalf("unexpected output kind for %s: got %q want %q", block.BlockName, got, want)
		}
		if got, want := output.Path, block.Outputs.Dirs[0]; got != want {
			t.Fatalf("unexpected output path for %s: got %q want %q", block.BlockName, got, want)
		}
		if got, want := output.VolumeSubpath, "dirs/0"; got != want {
			t.Fatalf("unexpected output subpath for %s: got %q want %q", block.BlockName, got, want)
		}
		if got, want := output.StorageRef, record.StorageRef; got != want {
			t.Fatalf("unexpected output storage ref for %s: got %q want %q", block.BlockName, got, want)
		}
		if got, want := output.SnapshotRef, "snapshot:"+block.BlockName; got != want {
			t.Fatalf("unexpected output snapshot ref for %s: got %q want %q", block.BlockName, got, want)
		}
	}
}

func TestCreateSandboxPublishesServiceBlockVolumeFileOutputs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	adapter := dependencyBlockVolumeRuntimeAdapter()
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml":          "go = \"1.26.2\"\n",
		"go.mod":             "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":             "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml": "services:\n  postgres:\n    image: postgres:17\n",
		"db/schema.sql":      "create table widgets (id serial primary key);\n",
		"scripts/prepare-db": "#!/bin/sh\ntrue\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	cacheStore := newMemoryCacheStore()
	svc.CacheStore = cacheStore

	policyProto := testRepositoryTwoDependencyBlocksPolicy()
	policyProto.Docker = &cleanroomv1.PolicyDocker{Required: true}
	policyProto.Services = &cleanroomv1.PolicyServices{
		Blocks: []*cleanroomv1.PolicyBlock{
			{
				Name:    "postgres-config",
				Command: []string{"sh", "-lc", "mkdir -p /var/lib/cleanroom/services && touch /var/lib/cleanroom/services/postgres.conf"},
				Inputs: &cleanroomv1.PolicyBlockInputs{
					Files: []string{"docker-compose.yml", "db/schema.sql", "scripts/prepare-db"},
				},
				Outputs: &cleanroomv1.PolicyBlockOutputs{
					Files: []string{"/var/lib/cleanroom/services/postgres.conf"},
				},
			},
		},
	}

	compiled, err := policy.FromProto(policyProto)
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	dependencyPlan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected dependency block-volume plan")
	}
	for _, block := range dependencyPlan.Blocks {
		if err := cacheStore.Create(context.Background(), dependencyBlockVolumeTestRecord(compiled, block)); err != nil {
			t.Fatalf("Create dependency block-volume record returned error: %v", err)
		}
	}
	servicePlan, ok, err := svc.finalizeServiceBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test", dependencyPlan)
	if err != nil {
		t.Fatalf("finalizeServiceBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected service block-volume plan")
	}
	block := servicePlan.Blocks[0]
	volumeID := blockVolumeID(serviceVolumeStageName, block.CacheKey)
	capturedFileOutput := false
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		if len(req.CacheOutputFileCaptures) > 0 {
			capturedFileOutput = true
			if got, want := req.CacheOutputFileCaptures, []backend.CacheOutputFileCapture{{
				VolumeID:      volumeID,
				GuestPath:     block.Outputs.Files[0],
				VolumeSubpath: "files/0",
			}}; !slices.Equal(got, want) {
				t.Fatalf("unexpected cache output file captures: got %#v want %#v", got, want)
			}
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0}, nil
	}
	adapter.snapshotCacheOutputsFn = func(_ context.Context, req backend.SnapshotCacheOutputVolumesRequest) (*backend.SnapshotCacheOutputVolumesResult, error) {
		if got, want := req.VolumeIDs, []string{volumeID}; !slices.Equal(got, want) {
			t.Fatalf("unexpected snapshot volume ids: got %v want %v", got, want)
		}
		return &backend.SnapshotCacheOutputVolumesResult{Volumes: []backend.CacheOutputVolumeSnapshot{
			{
				Stage:         serviceVolumeStageName,
				BlockName:     block.BlockName,
				CacheKey:      block.CacheKey,
				VolumeID:      volumeID,
				SnapshotID:    req.SnapshotIDPrefix + "-" + block.BlockName,
				StorageDriver: "file",
				StorageRef:    "/snapshots/" + block.BlockName + ".ext4",
				SnapshotRef:   "snapshot:" + block.BlockName,
				Outputs: []backend.CacheOutputVolumeSnapshotOutput{
					{
						Kind:          "file",
						GuestPath:     block.Outputs.Files[0],
						VolumeSubpath: "files/0",
					},
				},
			},
		}}, nil
	}

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             policyProto,
		RepositoryCheckout: repositoryCheckout,
	}); err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if !capturedFileOutput {
		t.Fatal("expected service block execution to capture declared file output")
	}
	if got, want := adapter.snapshotCacheOutputsCalls, 1; got != want {
		t.Fatalf("unexpected cache output snapshot calls: got %d want %d", got, want)
	}
	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	var record cachestore.Record
	for _, candidate := range records {
		if candidate.Stage == serviceVolumeStageName {
			record = candidate
		}
	}
	if record.CacheKey == "" {
		t.Fatal("expected service block-volume record for file-output block")
	}
	if got, want := len(record.OutputRecords), 1; got != want {
		t.Fatalf("unexpected output record count: got %d want %d", got, want)
	}
	output := record.OutputRecords[0]
	if got, want := output.Kind, "file"; got != want {
		t.Fatalf("unexpected output kind: got %q want %q", got, want)
	}
	if got, want := output.Path, block.Outputs.Files[0]; got != want {
		t.Fatalf("unexpected output path: got %q want %q", got, want)
	}
	if got, want := output.VolumeSubpath, "files/0"; got != want {
		t.Fatalf("unexpected output subpath: got %q want %q", got, want)
	}
}

func TestCreateSandboxLooksUpServiceBlockVolumesAfterDependencyStageRestore(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	adapter := dependencyBlockVolumeRuntimeAdapter()
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml":           "go = \"1.26.2\"\n",
		"go.mod":              "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":              "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml":  "services:\n  postgres:\n    image: postgres:17\n",
		"db/schema.sql":       "create table widgets (id serial primary key);\n",
		"db/seed.sql":         "insert into widgets default values;\n",
		"scripts/prepare-db":  "#!/bin/sh\ntrue\n",
		"scripts/prepare-app": "#!/bin/sh\ntrue\n",
	})
	svc := newTestServiceWithSnapshotStore(adapter, newMemorySnapshotStore())
	svc.RepositoryStore = mirrors
	cacheStore := &recordingCacheStore{inner: newMemoryCacheStore()}
	svc.CacheStore = cacheStore

	req := &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryTwoDependencyTwoServiceBlocksPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}
	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}

	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List cache records returned error: %v", err)
	}
	for _, record := range records {
		if record.Stage == servicesStageName {
			if err := cacheStore.Delete(context.Background(), record.Stage, record.CacheKey); err != nil {
				t.Fatalf("Delete services cache record returned error: %v", err)
			}
		}
	}

	compiled, err := policy.FromProto(testRepositoryTwoDependencyTwoServiceBlocksPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	aggregateDependencyPlan, aggregateDependencyEnabled := dependencyStagePlanForRepository(compiled, repository)
	if !aggregateDependencyEnabled {
		t.Fatal("expected aggregate dependency stage plan")
	}
	workspaceKey := workspaceStageCacheKey("firecracker", "runtime-base:test", compiled.Hash, repository, nil)
	aggregateDependencyPlan, aggregateDependencyEnabled, err = svc.finalizeDependencyStagePlan(context.Background(), compiled, repository, nil, nil, "firecracker", workspaceKey, "runtime-base:test", aggregateDependencyPlan)
	if err != nil {
		t.Fatalf("finalizeDependencyStagePlan returned error: %v", err)
	}
	if !aggregateDependencyEnabled {
		t.Fatal("expected finalized aggregate dependency stage plan")
	}
	if err := cacheStore.Create(context.Background(), cachestore.Record{
		CacheKey:          aggregateDependencyPlan.CacheKey,
		Stage:             dependencyStageName,
		ReuseMode:         dependencyStageReuseExact,
		State:             cacheStateReady,
		BackingSnapshotID: "snapshot-dependency",
		Backend:           "firecracker",
		PolicyHash:        compiled.Hash,
		Policy:            compiled.ToProto(),
		Repository:        cloneRepositoryCheckout(normalizeRepositoryCheckoutForComparison(repository)).ToProto(),
		ParentCacheKey:    aggregateDependencyPlan.ParentWorkspaceCacheKey,
		StorageRef:        "/snapshots/dependency.ext4",
		ProducerVersion:   dependencyStageProducerVersion,
	}); err != nil {
		t.Fatalf("Create aggregate dependency stage record returned error: %v", err)
	}

	dependencyPlan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected dependency block-volume plan")
	}
	servicePlan, ok, err := svc.finalizeServiceBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test", dependencyPlan)
	if err != nil {
		t.Fatalf("finalizeServiceBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected service block-volume plan")
	}
	for i := range servicePlan.Blocks {
		if err := cacheStore.Create(context.Background(), serviceBlockVolumeTestRecord(compiled, servicePlan.Blocks[i])); err != nil {
			t.Fatalf("Create service block-volume record %d returned error: %v", i, err)
		}
	}
	lookedUpServicePlan, err := svc.lookupServiceBlockVolumeCaches(context.Background(), "firecracker", compiled, servicePlan)
	if err != nil {
		t.Fatalf("lookupServiceBlockVolumeCaches returned error: %v", err)
	}
	cacheStore.resetLookups()

	secondResp, err := svc.CreateSandbox(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}
	if got, want := secondResp.GetSourceKind(), "dependency stage cache"; got != want {
		t.Fatalf("unexpected response source kind: got %q want %q", got, want)
	}
	if got, want := cacheStore.getReadyCount(serviceVolumeStageName), len(servicePlan.Blocks); got != want {
		t.Fatalf("unexpected service block-volume lookup count after dependency restore: got %d want %d", got, want)
	}
	if got, want := cacheStore.getReadyHitCount(serviceVolumeStageName), len(servicePlan.Blocks); got != want {
		t.Fatalf("unexpected service block-volume hit count after dependency restore: got %d want %d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotReq.CacheOutputVolumes, serviceBlockVolumeOutputSpecsForTest(t, lookedUpServicePlan); !cacheOutputVolumeSpecsEqual(got, want) {
		t.Fatalf("unexpected dependency-stage restore output volume specs:\ngot:  %#v\nwant: %#v", got, want)
	}
	if got, want := adapter.runCalls, 5; got != want {
		t.Fatalf("expected dependency block misses on first create plus aggregate services bootstrap after dependency restore, got %d want %d", got, want)
	}
}

func TestCreateSandboxPassesBlockVolumeSpecsToColdProvision(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	adapter := dependencyBlockVolumeRuntimeAdapter()
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml":           "go = \"1.26.2\"\n",
		"go.mod":              "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":              "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml":  "services:\n  postgres:\n    image: postgres:17\n",
		"db/schema.sql":       "create table widgets (id serial primary key);\n",
		"db/seed.sql":         "insert into widgets default values;\n",
		"scripts/prepare-db":  "#!/bin/sh\ntrue\n",
		"scripts/prepare-app": "#!/bin/sh\ntrue\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	cacheStore := newMemoryCacheStore()
	svc.CacheStore = cacheStore

	compiled, err := policy.FromProto(testRepositoryTwoDependencyTwoServiceBlocksPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	dependencyPlan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected dependency block-volume plan")
	}
	servicePlan, ok, err := svc.finalizeServiceBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test", dependencyPlan)
	if err != nil {
		t.Fatalf("finalizeServiceBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected service block-volume plan")
	}
	if err := cacheStore.Create(context.Background(), dependencyBlockVolumeTestRecord(compiled, dependencyPlan.Blocks[0])); err != nil {
		t.Fatalf("Create dependency block-volume record returned error: %v", err)
	}
	if err := cacheStore.Create(context.Background(), serviceBlockVolumeTestRecord(compiled, servicePlan.Blocks[0])); err != nil {
		t.Fatalf("Create service block-volume record returned error: %v", err)
	}
	lookedUpDependencyPlan, err := svc.lookupDependencyBlockVolumeCaches(context.Background(), "firecracker", compiled, dependencyPlan)
	if err != nil {
		t.Fatalf("lookupDependencyBlockVolumeCaches returned error: %v", err)
	}
	lookedUpServicePlan, err := svc.lookupServiceBlockVolumeCaches(context.Background(), "firecracker", compiled, servicePlan)
	if err != nil {
		t.Fatalf("lookupServiceBlockVolumeCaches returned error: %v", err)
	}

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryTwoDependencyTwoServiceBlocksPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}); err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	dependencySpecs := dependencyBlockVolumeOutputSpecsForTest(t, lookedUpDependencyPlan)
	serviceSpecs := serviceBlockVolumeOutputSpecsForTest(t, lookedUpServicePlan)
	want := appendCacheOutputVolumeSpecs(dependencySpecs, serviceSpecs)
	if got := adapter.provisionReq.CacheOutputVolumes; !cacheOutputVolumeSpecsEqual(got, want) {
		t.Fatalf("unexpected cold provision output volume specs:\ngot:  %#v\nwant: %#v", got, want)
	}
	hit := requireCacheOutputVolumeSpec(t, adapter.provisionReq.CacheOutputVolumes, dependencyVolumeStageName, dependencyPlan.Blocks[0].BlockName)
	if got, want := hit.SourceSnapshotRef, "snapshot:"+dependencyPlan.Blocks[0].BlockName; got != want {
		t.Fatalf("unexpected dependency hit snapshot ref: got %q want %q", got, want)
	}
	miss := requireCacheOutputVolumeSpec(t, adapter.provisionReq.CacheOutputVolumes, serviceVolumeStageName, servicePlan.Blocks[1].BlockName)
	if miss.StorageRef != "" || miss.SourceSnapshotRef != "" {
		t.Fatalf("expected service miss spec without source storage, got storage=%q snapshot=%q", miss.StorageRef, miss.SourceSnapshotRef)
	}
}

func TestCreateSandboxInvalidatesDependencyAndServiceBlockVolumesAfterSourceOnlyChange(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	adapter := dependencyBlockVolumeRuntimeAdapter()
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml":           "go = \"1.26.2\"\n",
		"go.mod":              "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":              "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml":  "services:\n  postgres:\n    image: postgres:17\n",
		"db/schema.sql":       "create table widgets (id serial primary key);\n",
		"db/seed.sql":         "insert into widgets default values;\n",
		"scripts/prepare-db":  "#!/bin/sh\ntrue\n",
		"scripts/prepare-app": "#!/bin/sh\ntrue\n",
		"app.go":              "package main\n\nfunc main() {}\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	cacheStore := &recordingCacheStore{inner: newMemoryCacheStore()}
	svc.CacheStore = cacheStore

	compiled, err := policy.FromProto(testRepositoryTwoDependencyTwoServiceBlocksPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	dependencyPlan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected dependency block-volume plan")
	}
	servicePlan, ok, err := svc.finalizeServiceBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test", dependencyPlan)
	if err != nil {
		t.Fatalf("finalizeServiceBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected service block-volume plan")
	}
	wantBlockExecutions := len(dependencyPlan.Blocks) + len(servicePlan.Blocks)

	var blockExecutions []backend.ExecutionRequest
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		if req.OverlayCapture != nil {
			blockExecutions = append(blockExecutions, req)
		}
		return &backend.ExecutionResult{
			ExecutionID:    req.ExecutionID,
			ExitCode:       0,
			LaunchedVM:     false,
			PlanPath:       "/tmp/plan",
			RunDir:         "/tmp/run",
			Message:        "ok",
			OverlayCapture: &backend.OverlayCaptureResult{},
		}, nil
	}
	adapter.snapshotCacheOutputsFn = func(_ context.Context, req backend.SnapshotCacheOutputVolumesRequest) (*backend.SnapshotCacheOutputVolumesResult, error) {
		specsByVolumeID := make(map[string]backend.CacheOutputVolumeSpec, len(adapter.provisionReq.CacheOutputVolumes))
		for _, spec := range adapter.provisionReq.CacheOutputVolumes {
			specsByVolumeID[spec.VolumeID] = spec
		}
		result := &backend.SnapshotCacheOutputVolumesResult{
			Volumes: make([]backend.CacheOutputVolumeSnapshot, 0, len(req.VolumeIDs)),
		}
		for _, volumeID := range req.VolumeIDs {
			spec, ok := specsByVolumeID[volumeID]
			if !ok {
				t.Fatalf("snapshot requested unknown cache output volume %q", volumeID)
			}
			result.Volumes = append(result.Volumes, blockVolumeReuseTestSnapshot(req.SnapshotIDPrefix, spec))
		}
		return result, nil
	}

	req := &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryTwoDependencyTwoServiceBlocksPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}
	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}
	if got, want := len(blockExecutions), wantBlockExecutions; got != want {
		t.Fatalf("expected first create to run %d dependency/service blocks, got %d", want, got)
	}
	if got, want := adapter.snapshotCacheOutputsCalls, wantBlockExecutions; got != want {
		t.Fatalf("expected first create to snapshot %d block volumes, got %d", want, got)
	}

	cacheStore.resetLookups()
	firstRunCalls := adapter.runCalls
	firstSnapshotCalls := adapter.snapshotCacheOutputsCalls
	if err := os.WriteFile(filepath.Join(mirrors.mirrorPath, "app.go"), []byte("package main\n\nfunc main() { println(\"changed\") }\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(app.go) returned error: %v", err)
	}
	runTestGit(t, mirrors.mirrorPath, "add", "app.go")
	runTestGit(t, mirrors.mirrorPath, "commit", "-m", "source-only change")
	updatedCheckout := *repositoryCheckout
	updatedCheckout.CommitSha = strings.TrimSpace(runTestGit(t, mirrors.mirrorPath, "rev-parse", "HEAD"))

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryTwoDependencyTwoServiceBlocksPolicy(),
		RepositoryCheckout: &updatedCheckout,
	}); err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}
	if got, want := adapter.runCalls, firstRunCalls+1+wantBlockExecutions; got != want {
		t.Fatalf("expected source-only create to rerun repository bootstrap and block executions, got total run calls %d want %d", got, want)
	}
	if got, want := len(blockExecutions), wantBlockExecutions*2; got != want {
		t.Fatalf("expected source-only create to rerun dependency/service block executions, got %d total block executions want %d", got, want)
	}
	if got, want := adapter.snapshotCacheOutputsCalls, firstSnapshotCalls+wantBlockExecutions; got != want {
		t.Fatalf("expected source-only create to republish block volumes for the new source identity, got snapshot calls %d want %d", got, want)
	}
	if got, want := cacheStore.getReadyCount(dependencyVolumeStageName), len(dependencyPlan.Blocks); got != want {
		t.Fatalf("expected source-only create to look up %d dependency block volumes, got %d", want, got)
	}
	if got := cacheStore.getReadyHitCount(dependencyVolumeStageName); got != 0 {
		t.Fatalf("expected source-only create to miss dependency block volumes for the new source identity, got %d hits", got)
	}
	if got, want := cacheStore.getReadyCount(serviceVolumeStageName), len(servicePlan.Blocks); got != want {
		t.Fatalf("expected source-only create to look up %d service block volumes, got %d", want, got)
	}
	if got := cacheStore.getReadyHitCount(serviceVolumeStageName); got != 0 {
		t.Fatalf("expected source-only create to miss service block volumes for the new source identity, got %d hits", got)
	}
	if got, want := len(adapter.provisionReq.CacheOutputVolumes), wantBlockExecutions; got != want {
		t.Fatalf("expected source-only create to attach %d fresh block volumes, got %d", want, got)
	}
	for _, spec := range adapter.provisionReq.CacheOutputVolumes {
		if strings.TrimSpace(spec.StorageRef) != "" || strings.TrimSpace(spec.SourceSnapshotRef) != "" {
			t.Fatalf("expected source-only create to use fresh volume %q for the new source identity, got storage=%q source_snapshot=%q", spec.VolumeID, spec.StorageRef, spec.SourceSnapshotRef)
		}
	}
}

func TestBootstrapServiceBlockVolumePlanRunsMissesFromWorkspaceOverlay(t *testing.T) {
	t.Parallel()

	compiled, err := policy.FromProto(testRepositoryTwoDependencyTwoServiceBlocksPolicy())
	if err != nil {
		t.Fatalf("policy.FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(testRepositoryCheckoutProto())
	plan := serviceBlockVolumePlan{
		Blocks: []serviceBlockVolumeBlockPlan{
			{
				BlockName: "postgres-data",
				Command:   append([]string(nil), compiled.Services.Blocks[0].Command...),
				Env:       cloneDependencyBlockEnv(compiled.Services.Blocks[0].Env),
				Inputs:    append([]string(nil), compiled.Services.Blocks[0].Inputs.Files...),
				CacheHit:  true,
			},
			{
				BlockName: "app-service",
				Command:   append([]string(nil), compiled.Services.Blocks[1].Command...),
				Env:       cloneDependencyBlockEnv(compiled.Services.Blocks[1].Env),
				Inputs:    append([]string(nil), compiled.Services.Blocks[1].Inputs.Files...),
				Outputs:   compiled.Services.Blocks[1].Outputs,
				CacheKey:  "service-volume:app-service",
			},
		},
	}

	var gotReqs []backend.ExecutionRequest
	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
			gotReqs = append(gotReqs, req)
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0}, nil
		},
	}
	svc := newTestService(adapter)
	publishable, err := svc.bootstrapServiceBlockVolumePlanInPersistentSandbox(
		context.Background(),
		adapter,
		"cr-test",
		serviceBlockVolumePublishConfig{},
		compiled,
		backend.FirecrackerConfig{},
		repository,
		plan,
		nil,
	)
	if err != nil {
		t.Fatalf("bootstrapServiceBlockVolumePlanInPersistentSandbox returned error: %v", err)
	}
	if !publishable {
		t.Fatal("expected service block volume plan to remain publishable")
	}
	if got, want := len(gotReqs), 1; got != want {
		t.Fatalf("unexpected run count: got %d want %d", got, want)
	}
	req := gotReqs[0]
	if got, want := strings.Join(req.Command, "\x00"), strings.Join(compiled.Services.Blocks[1].Command, "\x00"); got != want {
		t.Fatalf("unexpected command: got %q want %q", got, want)
	}
	if got, want := req.NetworkStage, policy.NetworkStageServices; got != want {
		t.Fatalf("unexpected network stage: got %q want %q", got, want)
	}
	if got, want := req.Dir, "/workspace"; got != want {
		t.Fatalf("unexpected dir: got %q want %q", got, want)
	}
	if !req.ClosedEnv {
		t.Fatal("expected service block execution to use a closed environment")
	}
	if !strings.Contains(strings.Join(req.Env, "\n"), "APP_SERVICE_DATA=/var/lib/cleanroom/services/app") {
		t.Fatalf("expected APP_SERVICE_DATA env, got %v", req.Env)
	}
	if req.InputProjection != nil {
		t.Fatalf("expected service block miss to run against the normal workspace, got projection %#v", req.InputProjection)
	}
	if req.OverlayCapture == nil {
		t.Fatal("expected overlay capture request")
	}
	if got, want := req.OverlayCapture.UpperDir, filepath.Join(blockVolumeOverlayCaptureRoot, blockVolumeID(serviceVolumeStageName, plan.Blocks[1].CacheKey), "upper"); got != want {
		t.Fatalf("unexpected overlay capture upper dir: got %q want %q", got, want)
	}
	if !slices.Equal(req.OverlayCapture.BaselinePaths, plan.Blocks[1].Outputs.Dirs) {
		t.Fatalf("unexpected overlay capture baseline paths: got %v want %v", req.OverlayCapture.BaselinePaths, plan.Blocks[1].Outputs.Dirs)
	}
	if !slices.Equal(req.OverlayCapture.DeclaredFileOutputs, plan.Blocks[1].Outputs.Files) {
		t.Fatalf("unexpected overlay capture file outputs: got %v want %v", req.OverlayCapture.DeclaredFileOutputs, plan.Blocks[1].Outputs.Files)
	}
	if !slices.Equal(req.OverlayCapture.IgnoredPrefixes, blockVolumeOverlayCaptureIgnoredPrefixes) {
		t.Fatalf("unexpected overlay capture ignored prefixes: got %v want %v", req.OverlayCapture.IgnoredPrefixes, blockVolumeOverlayCaptureIgnoredPrefixes)
	}
}

func TestBootstrapServiceBlockVolumePlanStopsPublishingAfterEscapedWrites(t *testing.T) {
	compiled, err := policy.FromProto(testRepositoryTwoDependencyTwoServiceBlocksPolicy())
	if err != nil {
		t.Fatalf("policy.FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(testRepositoryCheckoutProto())
	plan := serviceBlockVolumePlan{
		Blocks: []serviceBlockVolumeBlockPlan{
			{
				BlockName: "postgres-data",
				Command:   append([]string(nil), compiled.Services.Blocks[0].Command...),
				Env:       cloneDependencyBlockEnv(compiled.Services.Blocks[0].Env),
				Inputs:    append([]string(nil), compiled.Services.Blocks[0].Inputs.Files...),
				Outputs:   compiled.Services.Blocks[0].Outputs,
				CacheKey:  "service-volume:postgres-data",
			},
			{
				BlockName: "app-service",
				Command:   append([]string(nil), compiled.Services.Blocks[1].Command...),
				Env:       cloneDependencyBlockEnv(compiled.Services.Blocks[1].Env),
				Inputs:    append([]string(nil), compiled.Services.Blocks[1].Inputs.Files...),
				Outputs:   compiled.Services.Blocks[1].Outputs,
				CacheKey:  "service-volume:app-service",
				CacheHit:  true,
			},
		},
	}

	var gotReqs []backend.ExecutionRequest
	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
			gotReqs = append(gotReqs, req)
			result := &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0}
			if len(gotReqs) == 1 {
				result.OverlayCapture = &backend.OverlayCaptureResult{
					EscapedWrites: []backend.OverlayCaptureEntry{{Path: "/usr/local/bin/tool", Kind: "write", Mode: 0o755}},
				}
			}
			return result, nil
		},
		snapshotCacheOutputsFn: func(context.Context, backend.SnapshotCacheOutputVolumesRequest) (*backend.SnapshotCacheOutputVolumesResult, error) {
			t.Fatal("did not expect service block-volume snapshot after escaped writes")
			return nil, nil
		},
	}
	svc := newTestService(adapter)
	var warnings []string
	publishable, err := svc.bootstrapServiceBlockVolumePlanInPersistentSandbox(
		context.Background(),
		adapter,
		"cr-test",
		serviceBlockVolumePublishConfig{Adapter: adapter, Backend: "firecracker", Repository: repository},
		compiled,
		backend.FirecrackerConfig{},
		repository,
		plan,
		CreateSandboxReporterFuncs{OnWarning: func(_ cleanroomv1.CreateSandboxPhase, warning string) {
			warnings = append(warnings, warning)
		}},
	)
	if err != nil {
		t.Fatalf("bootstrapServiceBlockVolumePlanInPersistentSandbox returned error: %v", err)
	}
	if publishable {
		t.Fatal("expected service block volume plan to stop being publishable")
	}
	if got, want := len(gotReqs), 5; got != want {
		t.Fatalf("unexpected run count: got %d want %d", got, want)
	}
	if gotReqs[0].OverlayCapture == nil {
		t.Fatal("expected first service block attempt to use overlay capture")
	}
	assertBlockVolumeResetRequest(t, gotReqs[1], policy.NetworkStageServices)
	assertBlockVolumeFallbackRequest(t, gotReqs[2], compiled.Services.Blocks[0].Command, policy.NetworkStageServices)
	assertBlockVolumeResetRequest(t, gotReqs[3], policy.NetworkStageServices)
	assertBlockVolumeFallbackRequest(t, gotReqs[4], compiled.Services.Blocks[1].Command, policy.NetworkStageServices)
	if got := adapter.snapshotCacheOutputsCalls; got != 0 {
		t.Fatalf("unexpected cache output snapshot calls: got %d", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "/usr/local/bin/tool") {
		t.Fatalf("expected escaped-write warning, got %v", warnings)
	}
}

func TestBootstrapServiceBlockVolumePlanForceExactFallbackIgnoresCacheHits(t *testing.T) {
	compiled, err := policy.FromProto(testRepositoryTwoDependencyTwoServiceBlocksPolicy())
	if err != nil {
		t.Fatalf("policy.FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(testRepositoryCheckoutProto())
	plan := serviceBlockVolumePlan{
		Blocks: []serviceBlockVolumeBlockPlan{
			{
				BlockName: "postgres-data",
				Command:   append([]string(nil), compiled.Services.Blocks[0].Command...),
				Env:       cloneDependencyBlockEnv(compiled.Services.Blocks[0].Env),
				Inputs:    append([]string(nil), compiled.Services.Blocks[0].Inputs.Files...),
				Outputs:   compiled.Services.Blocks[0].Outputs,
				CacheKey:  "service-volume:postgres-data",
				CacheHit:  true,
			},
			{
				BlockName: "app-service",
				Command:   append([]string(nil), compiled.Services.Blocks[1].Command...),
				Env:       cloneDependencyBlockEnv(compiled.Services.Blocks[1].Env),
				Inputs:    append([]string(nil), compiled.Services.Blocks[1].Inputs.Files...),
				Outputs:   compiled.Services.Blocks[1].Outputs,
				CacheKey:  "service-volume:app-service",
			},
		},
	}

	var gotReqs []backend.ExecutionRequest
	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
			gotReqs = append(gotReqs, req)
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0}, nil
		},
		snapshotCacheOutputsFn: func(context.Context, backend.SnapshotCacheOutputVolumesRequest) (*backend.SnapshotCacheOutputVolumesResult, error) {
			t.Fatal("did not expect service block-volume snapshot during forced fallback")
			return nil, nil
		},
	}
	svc := newTestService(adapter)
	publishable, err := svc.bootstrapServiceBlockVolumePlanInPersistentSandbox(
		context.Background(),
		adapter,
		"cr-test",
		serviceBlockVolumePublishConfig{ForceExactFallback: true},
		compiled,
		backend.FirecrackerConfig{},
		repository,
		plan,
		nil,
	)
	if err != nil {
		t.Fatalf("bootstrapServiceBlockVolumePlanInPersistentSandbox returned error: %v", err)
	}
	if publishable {
		t.Fatal("expected forced fallback to remain unpublishable")
	}
	if got, want := len(gotReqs), 4; got != want {
		t.Fatalf("unexpected run count: got %d want %d", got, want)
	}
	assertBlockVolumeResetRequest(t, gotReqs[0], policy.NetworkStageServices)
	assertBlockVolumeFallbackRequest(t, gotReqs[1], compiled.Services.Blocks[0].Command, policy.NetworkStageServices)
	assertBlockVolumeResetRequest(t, gotReqs[2], policy.NetworkStageServices)
	assertBlockVolumeFallbackRequest(t, gotReqs[3], compiled.Services.Blocks[1].Command, policy.NetworkStageServices)
	if got := adapter.snapshotCacheOutputsCalls; got != 0 {
		t.Fatalf("unexpected cache output snapshot calls: got %d", got)
	}
}

func TestCreateSandboxSkipsServiceBlockVolumePublicationAfterDependencyEscapedWrites(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	adapter := dependencyBlockVolumeRuntimeAdapter()
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml":           "go = \"1.26.2\"\n",
		"go.mod":              "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":              "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml":  "services:\n  postgres:\n    image: postgres:17\n",
		"db/schema.sql":       "create table widgets (id serial primary key);\n",
		"db/seed.sql":         "insert into widgets default values;\n",
		"scripts/prepare-db":  "#!/bin/sh\ntrue\n",
		"scripts/prepare-app": "#!/bin/sh\ntrue\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	cacheStore := newMemoryCacheStore()
	svc.CacheStore = cacheStore

	dependencyEscaped := false
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		result := &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0}
		if req.NetworkStage == policy.NetworkStageDependencies && req.ClosedEnv && !dependencyEscaped {
			dependencyEscaped = true
			result.OverlayCapture = &backend.OverlayCaptureResult{
				EscapedWrites: []backend.OverlayCaptureEntry{{Path: "/etc/profile", Kind: "write", Mode: 0o644}},
			}
		}
		return result, nil
	}
	adapter.snapshotCacheOutputsFn = func(context.Context, backend.SnapshotCacheOutputVolumesRequest) (*backend.SnapshotCacheOutputVolumesResult, error) {
		t.Fatal("did not expect dependency or service block-volume snapshots after dependency escaped writes")
		return nil, nil
	}

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryTwoDependencyTwoServiceBlocksPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}); err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if !dependencyEscaped {
		t.Fatal("expected dependency block execution to report escaped writes")
	}
	if got := adapter.snapshotCacheOutputsCalls; got != 0 {
		t.Fatalf("unexpected cache output snapshot calls: got %d", got)
	}
	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	for _, record := range records {
		if record.Stage == dependencyVolumeStageName || record.Stage == serviceVolumeStageName {
			t.Fatalf("did not expect block-volume cache record after dependency escaped writes, got %#v", record)
		}
	}
}

func dependencyBlockVolumeOutputSpecsForTest(t *testing.T, plan dependencyBlockVolumePlan) []backend.CacheOutputVolumeSpec {
	t.Helper()
	specs, err := dependencyBlockVolumeOutputSpecs(plan)
	if err != nil {
		t.Fatalf("dependencyBlockVolumeOutputSpecs returned error: %v", err)
	}
	return specs
}

func serviceBlockVolumeOutputSpecsForTest(t *testing.T, plan serviceBlockVolumePlan) []backend.CacheOutputVolumeSpec {
	t.Helper()
	specs, err := serviceBlockVolumeOutputSpecs(plan)
	if err != nil {
		t.Fatalf("serviceBlockVolumeOutputSpecs returned error: %v", err)
	}
	return specs
}

func cacheOutputVolumeSpecsEqual(left, right []backend.CacheOutputVolumeSpec) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Stage != right[i].Stage ||
			left[i].BlockName != right[i].BlockName ||
			left[i].CacheKey != right[i].CacheKey ||
			left[i].VolumeID != right[i].VolumeID ||
			left[i].SourceSnapshotRef != right[i].SourceSnapshotRef ||
			left[i].StorageDriver != right[i].StorageDriver ||
			left[i].StorageRef != right[i].StorageRef {
			return false
		}
		if len(left[i].DirMappings) != len(right[i].DirMappings) || len(left[i].FileMappings) != len(right[i].FileMappings) {
			return false
		}
		for j := range left[i].DirMappings {
			if left[i].DirMappings[j] != right[i].DirMappings[j] {
				return false
			}
		}
		for j := range left[i].FileMappings {
			if left[i].FileMappings[j] != right[i].FileMappings[j] {
				return false
			}
		}
	}
	return true
}

func testRepositoryTwoDependencyTwoServiceBlocksPolicy() *cleanroomv1.Policy {
	policyProto := testRepositoryTwoDependencyBlocksPolicy()
	policyProto.Docker = &cleanroomv1.PolicyDocker{Required: true}
	policyProto.Services = &cleanroomv1.PolicyServices{
		Blocks: []*cleanroomv1.PolicyBlock{
			{
				Name:    "postgres-data",
				Command: []string{"./scripts/prepare-db"},
				Inputs: &cleanroomv1.PolicyBlockInputs{
					Files: []string{"docker-compose.yml", "db/schema.sql", "scripts/prepare-db"},
				},
				Env: map[string]string{
					"PGDATA": "/var/lib/cleanroom/services/postgres",
				},
				Outputs: &cleanroomv1.PolicyBlockOutputs{
					Dirs: []string{"/var/lib/cleanroom/services/postgres"},
				},
			},
			{
				Name:    "app-service",
				Command: []string{"./scripts/prepare-app"},
				Inputs: &cleanroomv1.PolicyBlockInputs{
					Files: []string{"docker-compose.yml", "db/seed.sql", "scripts/prepare-app"},
				},
				Env: map[string]string{
					"APP_SERVICE_DATA": "/var/lib/cleanroom/services/app",
				},
				Outputs: &cleanroomv1.PolicyBlockOutputs{
					Dirs: []string{"/var/lib/cleanroom/services/app"},
				},
			},
		},
	}
	return policyProto
}

func serviceBlockVolumeTestRecord(compiled *policy.CompiledPolicy, block serviceBlockVolumeBlockPlan) cachestore.Record {
	return cachestore.Record{
		CacheKey:                block.CacheKey,
		Stage:                   serviceVolumeStageName,
		State:                   cacheStateReady,
		BackingSnapshotID:       "service-volume-snapshot",
		Backend:                 "firecracker",
		PolicyHash:              compiled.Hash,
		Policy:                  compiled.ToProto(),
		StorageDriver:           "file",
		StorageRef:              "/snapshots/" + block.BlockName + ".ext4",
		InputManifestDigest:     block.InputManifestDigest,
		CommandDigest:           block.CommandDigest,
		EnvDigest:               block.EnvDigest,
		NormalizedOutputsDigest: block.NormalizedOutputsDigest,
		OutputRecords: []cachestore.OutputRecord{
			{
				Kind:          "dir",
				Path:          block.Outputs.Dirs[0],
				VolumeSubpath: "dirs/0",
				StorageDriver: "file",
				StorageRef:    "/snapshots/" + block.BlockName + ".ext4",
				SnapshotRef:   "snapshot:" + block.BlockName,
			},
		},
		ProducerVersion: block.ProducerVersion,
	}
}

func blockVolumeReuseTestSnapshot(snapshotIDPrefix string, spec backend.CacheOutputVolumeSpec) backend.CacheOutputVolumeSnapshot {
	storageRef := "/snapshots/" + spec.VolumeID + ".ext4"
	snapshot := backend.CacheOutputVolumeSnapshot{
		Stage:         spec.Stage,
		BlockName:     spec.BlockName,
		CacheKey:      spec.CacheKey,
		VolumeID:      spec.VolumeID,
		SnapshotID:    snapshotIDPrefix + "-" + spec.BlockName,
		StorageDriver: "file",
		StorageRef:    storageRef,
		SnapshotRef:   "snapshot:" + spec.BlockName,
	}
	for _, dir := range spec.DirMappings {
		snapshot.Outputs = append(snapshot.Outputs, backend.CacheOutputVolumeSnapshotOutput{
			Kind:          "dir",
			GuestPath:     dir.GuestPath,
			VolumeSubpath: dir.Subpath,
		})
	}
	for _, file := range spec.FileMappings {
		snapshot.Outputs = append(snapshot.Outputs, backend.CacheOutputVolumeSnapshotOutput{
			Kind:          "file",
			GuestPath:     file.GuestPath,
			VolumeSubpath: file.Subpath,
		})
	}
	return snapshot
}
