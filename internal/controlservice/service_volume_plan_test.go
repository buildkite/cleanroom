package controlservice

import (
	"context"
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
			"command": block.CommandDigest,
			"env":     block.EnvDigest,
			"inputs":  block.InputManifestDigest,
			"outputs": block.NormalizedOutputsDigest,
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
		name     string
		records  int
		wantHits int
	}{
		{
			name:     "partial hit",
			records:  1,
			wantHits: 1,
		},
		{
			name:     "all hit",
			records:  2,
			wantHits: 2,
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
			if got, want := adapter.runCalls, 4; got != want {
				t.Fatalf("expected repository + dependency block misses + aggregate services bootstrap executions, got %d want %d", got, want)
			}
		})
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
