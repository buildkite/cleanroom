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

func TestFinalizeDependencyBlockVolumePlanBuildsOrderedKeys(t *testing.T) {
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml": "go = \"1.26.2\"\n",
		"go.mod":    "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":    "example.com/test v0.0.0 h1:abc123\n",
	})
	svc := newTestService(&stubAdapter{})
	svc.RepositoryStore = mirrors

	compiled, err := policy.FromProto(testRepositoryTwoDependencyBlocksPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)

	plan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected dependency block volume plan")
	}
	if got, want := plan.ReuseNamespace, "https://github.com/buildkite/cleanroom.git"; got != want {
		t.Fatalf("unexpected reuse namespace: got %q want %q", got, want)
	}
	if got, want := len(plan.Blocks), 2; got != want {
		t.Fatalf("unexpected dependency block count: got %d want %d", got, want)
	}

	first := plan.Blocks[0]
	second := plan.Blocks[1]
	if first.CacheKey == "" || second.CacheKey == "" {
		t.Fatalf("expected non-empty block cache keys: first=%q second=%q", first.CacheKey, second.CacheKey)
	}
	if first.CacheKey == second.CacheKey {
		t.Fatalf("expected distinct dependency block cache keys, got %q", first.CacheKey)
	}
	emptyPriorDigest, err := digestCanonicalJSON([]string{})
	if err != nil {
		t.Fatalf("digest empty prior keys: %v", err)
	}
	if got := first.PriorDependencyOutputKeysDigest; got != emptyPriorDigest {
		t.Fatalf("unexpected first prior output digest: got %q want %q", got, emptyPriorDigest)
	}
	firstKeyDigest, err := digestCanonicalJSON([]string{first.CacheKey})
	if err != nil {
		t.Fatalf("digest first cache key: %v", err)
	}
	if got := second.PriorDependencyOutputKeysDigest; got != firstKeyDigest {
		t.Fatalf("unexpected second prior output digest: got %q want %q", got, firstKeyDigest)
	}
	replannedSecond, err := svc.finalizeDependencyBlockVolumeBlockPlan(
		context.Background(),
		compiled,
		repository,
		nil,
		nil,
		"firecracker",
		"runtime-base:test",
		plan.ReuseNamespace,
		compiled.Dependencies.Blocks[1],
		[]string{"dependency-volume:v1:different"},
	)
	if err != nil {
		t.Fatalf("finalize second block with alternate prior key returned error: %v", err)
	}
	if got, want := replannedSecond.CacheKey, second.CacheKey; got == want {
		t.Fatalf("expected second block key to change when prior dependency output key changes, got %q", got)
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

	mutatedPolicy := testRepositoryTwoDependencyBlocksPolicy()
	mutatedPolicy.Dependencies.Blocks[0].Env["MISE_DATA_DIR"] = "/root/.local/share/mise-other"
	mutatedCompiled, err := policy.FromProto(mutatedPolicy)
	if err != nil {
		t.Fatalf("FromProto mutated policy returned error: %v", err)
	}
	mutatedPlan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), mutatedCompiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan mutated returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected mutated dependency block volume plan")
	}
	if got, want := mutatedPlan.Blocks[0].CacheKey, first.CacheKey; got == want {
		t.Fatalf("expected first block key to change after env mutation, got %q", got)
	}

	mutatedRepository := *repository
	mutatedRepository.DestinationDir = "/src"
	mutatedPlan, ok, err = svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, &mutatedRepository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan destination mutation returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected destination-mutated dependency block volume plan")
	}
	if got, want := mutatedPlan.Blocks[0].CacheKey, first.CacheKey; got == want {
		t.Fatalf("expected first block key to change after destination dir mutation, got %q", got)
	}
}

func TestFinalizeDependencyBlockVolumePlanRejectsSymlinkInput(t *testing.T) {
	repoDir := t.TempDir()
	runTestGit(t, repoDir, "init")
	runTestGit(t, repoDir, "config", "user.email", "test@example.com")
	runTestGit(t, repoDir, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repoDir, "real.lock"), []byte("real\n"), 0o644); err != nil {
		t.Fatalf("write real.lock: %v", err)
	}
	if err := os.Symlink("real.lock", filepath.Join(repoDir, "link.lock")); err != nil {
		t.Fatalf("symlink link.lock: %v", err)
	}
	runTestGit(t, repoDir, "add", ".")
	runTestGit(t, repoDir, "commit", "-m", "test")
	repositoryCheckout := &cleanroomv1.RepositoryCheckout{
		RemoteUrl:      "https://github.com/buildkite/cleanroom.git",
		CommitSha:      strings.TrimSpace(runTestGit(t, repoDir, "rev-parse", "HEAD")),
		DestinationDir: "/workspace",
	}

	svc := newTestService(&stubAdapter{})
	svc.RepositoryStore = &stubRepositoryMirrorStore{mirrorPath: repoDir}
	policyProto := testRepositoryTwoDependencyBlocksPolicy()
	policyProto.Dependencies.Blocks[0].Inputs.Files = []string{"link.lock"}
	compiled, err := policy.FromProto(policyProto)
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)

	_, _, err = svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err == nil {
		t.Fatal("expected symlink input to fail")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink error, got %v", err)
	}
}

func TestLookupDependencyBlockVolumeCachesReportsPartialHit(t *testing.T) {
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml": "go = \"1.26.2\"\n",
		"go.mod":    "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":    "example.com/test v0.0.0 h1:abc123\n",
	})
	svc := newTestService(&stubAdapter{})
	svc.RepositoryStore = mirrors

	compiled, err := policy.FromProto(testRepositoryTwoDependencyBlocksPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	plan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected dependency block volume plan")
	}

	first := plan.Blocks[0]
	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	if err := cacheStore.Create(context.Background(), dependencyBlockVolumeTestRecord(compiled, first)); err != nil {
		t.Fatalf("Create cache record returned error: %v", err)
	}

	lookedUp, err := svc.lookupDependencyBlockVolumeCaches(context.Background(), "firecracker", compiled, plan)
	if err != nil {
		t.Fatalf("lookupDependencyBlockVolumeCaches returned error: %v", err)
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

func TestLookupDependencyBlockVolumeCachesRejectsMismatchedOutputRecords(t *testing.T) {
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml": "go = \"1.26.2\"\n",
		"go.mod":    "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":    "example.com/test v0.0.0 h1:abc123\n",
	})
	svc := newTestService(&stubAdapter{})
	svc.RepositoryStore = mirrors

	compiled, err := policy.FromProto(testRepositoryTwoDependencyBlocksPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	plan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected dependency block volume plan")
	}

	record := dependencyBlockVolumeTestRecord(compiled, plan.Blocks[0])
	record.OutputRecords[0].Path = "/root/other"
	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	if err := cacheStore.Create(context.Background(), record); err != nil {
		t.Fatalf("Create cache record returned error: %v", err)
	}

	lookedUp, err := svc.lookupDependencyBlockVolumeCaches(context.Background(), "firecracker", compiled, plan)
	if err != nil {
		t.Fatalf("lookupDependencyBlockVolumeCaches returned error: %v", err)
	}
	if lookedUp.Blocks[0].CacheHit {
		t.Fatal("did not expect cache hit with mismatched output record path")
	}
	if got, want := lookedUp.Blocks[0].LookupReason, observability.CacheLookupReasonRecordNotFound; got != want {
		t.Fatalf("unexpected lookup reason: got %q want %q", got, want)
	}
}

func TestLookupDependencyBlockVolumeCachesTreatsMissingStoreAsMiss(t *testing.T) {
	svc := newTestService(&stubAdapter{})
	svc.CacheStore = nil
	plan := dependencyBlockVolumePlan{
		ReuseNamespace: "https://github.com/buildkite/cleanroom.git",
		Blocks: []dependencyBlockVolumeBlockPlan{
			{
				BlockName: "go-modules",
				CacheKey:  "dependency-volume:v1:test",
			},
		},
	}

	got, err := svc.lookupDependencyBlockVolumeCaches(context.Background(), "firecracker", &policy.CompiledPolicy{Hash: "sha256:test"}, plan)
	if err != nil {
		t.Fatalf("lookupDependencyBlockVolumeCaches returned error: %v", err)
	}
	if got.Blocks[0].CacheHit {
		t.Fatal("did not expect cache hit when cache store is unavailable")
	}
	if got.Blocks[0].LookupReason != "" {
		t.Fatalf("expected missing cache store to leave lookup reason unchanged, got %q", got.Blocks[0].LookupReason)
	}
}

func TestCreateSandboxSkipsDependencyBlockVolumeLookupWhenBackendUnsupported(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml": "go = \"1.26.2\"\n",
		"go.mod":    "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":    "example.com/test v0.0.0 h1:abc123\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	cacheStore := &recordingCacheStore{inner: newMemoryCacheStore()}
	svc.CacheStore = cacheStore

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryTwoDependencyBlocksPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}); err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got := cacheStore.getReadyCount(dependencyVolumeStageName); got != 0 {
		t.Fatalf("expected unsupported backend to skip dependency block-volume cache lookups, got %d", got)
	}
	if got, want := adapter.runCalls, 2; got != want {
		t.Fatalf("expected aggregate repository + dependency bootstrap executions, got %d want %d", got, want)
	}
}

func TestCreateSandboxFallsBackWhenDependencyBlockVolumeStoreMissing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	adapter := dependencyBlockVolumeRuntimeAdapter()
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml": "go = \"1.26.2\"\n",
		"go.mod":    "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":    "example.com/test v0.0.0 h1:abc123\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	svc.CacheStore = nil

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryTwoDependencyBlocksPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}); err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got, want := adapter.runCalls, 2; got != want {
		t.Fatalf("expected aggregate repository + dependency bootstrap executions, got %d want %d", got, want)
	}
}

func TestCreateSandboxLooksUpDependencyBlockVolumeCaches(t *testing.T) {
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
				"mise.toml": "go = \"1.26.2\"\n",
				"go.mod":    "module example.com/test\n\ngo 1.26.2\n",
				"go.sum":    "example.com/test v0.0.0 h1:abc123\n",
			})
			svc := newTestService(adapter)
			svc.RepositoryStore = mirrors
			cacheStore := &recordingCacheStore{inner: newMemoryCacheStore()}
			svc.CacheStore = cacheStore

			compiled, err := policy.FromProto(testRepositoryTwoDependencyBlocksPolicy())
			if err != nil {
				t.Fatalf("FromProto returned error: %v", err)
			}
			repository := repositorycheckout.FromProto(repositoryCheckout)
			plan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
			if err != nil {
				t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
			}
			if !ok {
				t.Fatal("expected dependency block-volume plan")
			}
			for i := 0; i < tc.records; i++ {
				if err := cacheStore.Create(context.Background(), dependencyBlockVolumeTestRecord(compiled, plan.Blocks[i])); err != nil {
					t.Fatalf("Create dependency block-volume record %d returned error: %v", i, err)
				}
			}

			if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
				Policy:             testRepositoryTwoDependencyBlocksPolicy(),
				RepositoryCheckout: repositoryCheckout,
			}); err != nil {
				t.Fatalf("CreateSandbox returned error: %v", err)
			}
			if got, want := cacheStore.getReadyCount(dependencyVolumeStageName), len(plan.Blocks); got != want {
				t.Fatalf("unexpected dependency block-volume lookup count: got %d want %d", got, want)
			}
			if got := cacheStore.getReadyHitCount(dependencyVolumeStageName); got != tc.wantHits {
				t.Fatalf("unexpected dependency block-volume hit count: got %d want %d", got, tc.wantHits)
			}
			gotKeys := cacheStore.getReadyKeys(dependencyVolumeStageName)
			for i, wantKey := range []string{plan.Blocks[0].CacheKey, plan.Blocks[1].CacheKey} {
				if got := gotKeys[i]; got != wantKey {
					t.Fatalf("unexpected lookup key %d: got %q want %q", i, got, wantKey)
				}
			}
			if got, want := adapter.runCalls, 1+len(plan.Blocks)-tc.wantHits; got != want {
				t.Fatalf("expected repository plus dependency block miss executions, got %d want %d", got, want)
			}
		})
	}
}

func TestCreateSandboxPublishesDependencyBlockVolumeCachesForMisses(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	adapter := dependencyBlockVolumeRuntimeAdapter()
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml": "go = \"1.26.2\"\n",
		"go.mod":    "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":    "example.com/test v0.0.0 h1:abc123\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	compiled, err := policy.FromProto(testRepositoryTwoDependencyBlocksPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	plan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected dependency block-volume plan")
	}
	blockByVolumeID := make(map[string]dependencyBlockVolumeBlockPlan, len(plan.Blocks))
	wantVolumeIDs := make([]string, 0, len(plan.Blocks))
	for _, block := range plan.Blocks {
		volumeID := blockVolumeID(dependencyVolumeStageName, block.CacheKey)
		blockByVolumeID[volumeID] = block
		wantVolumeIDs = append(wantVolumeIDs, volumeID)
	}
	gotSnapshotVolumeIDs := make([]string, 0, len(plan.Blocks))

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
				Stage:              dependencyVolumeStageName,
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
						VolumeSubpath: "dir/0",
					},
				},
			},
		}}, nil
	}

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryTwoDependencyBlocksPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}); err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got, want := adapter.snapshotCacheOutputsCalls, len(plan.Blocks); got != want {
		t.Fatalf("unexpected cache output snapshot calls: got %d want %d", got, want)
	}
	if !slices.Equal(gotSnapshotVolumeIDs, wantVolumeIDs) {
		t.Fatalf("unexpected snapshot volume ids: got %v want %v", gotSnapshotVolumeIDs, wantVolumeIDs)
	}
	if got, want := adapter.runCalls, 1+len(plan.Blocks); got != want {
		t.Fatalf("expected repository plus dependency block miss executions, got %d want %d", got, want)
	}

	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	recordsByKey := make(map[string]cachestore.Record)
	for _, record := range records {
		if record.Stage == dependencyVolumeStageName {
			recordsByKey[record.CacheKey] = record
		}
	}
	if got, want := len(recordsByKey), len(plan.Blocks); got != want {
		t.Fatalf("unexpected dependency block-volume record count: got %d want %d", got, want)
	}
	for _, block := range plan.Blocks {
		record, ok := recordsByKey[block.CacheKey]
		if !ok {
			t.Fatalf("missing dependency block-volume record for %q", block.BlockName)
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
		if got, want := output.VolumeSubpath, "dir/0"; got != want {
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

func TestCreateSandboxPublishesDependencyBlockVolumeFileOutputs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	adapter := dependencyBlockVolumeRuntimeAdapter()
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"mise.toml": "go = \"1.26.2\"\n",
		"tool.lock": "tool v1\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	cacheStore := newMemoryCacheStore()
	svc.CacheStore = cacheStore

	policyProto := testRepositoryPolicy()
	policyProto.Dependencies = &cleanroomv1.PolicyDependencies{
		Blocks: []*cleanroomv1.PolicyBlock{
			{
				Name:    "tool-index",
				Command: []string{"sh", "-lc", "mkdir -p /root/.cache/tool && touch /root/.cache/tool/index.json"},
				Inputs:  &cleanroomv1.PolicyBlockInputs{Files: []string{"mise.toml", "tool.lock"}},
				Outputs: &cleanroomv1.PolicyBlockOutputs{
					Files: []string{"/root/.cache/tool/index.json"},
				},
			},
		},
	}
	compiled, err := policy.FromProto(policyProto)
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	plan, ok, err := svc.finalizeDependencyBlockVolumePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "runtime-base:test")
	if err != nil {
		t.Fatalf("finalizeDependencyBlockVolumePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected dependency block-volume plan")
	}
	block := plan.Blocks[0]
	volumeID := blockVolumeID(dependencyVolumeStageName, block.CacheKey)
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
				Stage:         dependencyVolumeStageName,
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
		t.Fatal("expected dependency block execution to capture declared file output")
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
		if candidate.Stage == dependencyVolumeStageName {
			record = candidate
		}
	}
	if record.CacheKey == "" {
		t.Fatal("expected dependency block-volume record for file-output block")
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

func TestDependencyBlockVolumeRuntimeDecisionRequiresOutputVolumesAndOverlay(t *testing.T) {
	tests := []struct {
		name       string
		adapter    backend.Adapter
		wantEnable bool
		wantReason string
	}{
		{
			name:       "missing output volume capability",
			adapter:    &stubAdapter{},
			wantReason: backend.CapabilitySandboxCacheOutputVolumes,
		},
		{
			name: "missing overlay capture capability",
			adapter: &portDialAdapter{capabilities: map[string]bool{
				backend.CapabilitySandboxCacheOutputVolumes: true,
			}},
			wantReason: backend.CapabilitySandboxOverlayWriteCapture,
		},
		{
			name: "supported",
			adapter: &portDialAdapter{capabilities: map[string]bool{
				backend.CapabilitySandboxCacheOutputVolumes:  true,
				backend.CapabilitySandboxOverlayWriteCapture: true,
			}},
			wantEnable: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decision := dependencyBlockVolumeRuntimeDecisionForAdapter(tc.adapter)
			if got := decision.Enabled; got != tc.wantEnable {
				t.Fatalf("unexpected decision enabled: got %v want %v (reason=%q)", got, tc.wantEnable, decision.FallbackReason)
			}
			if tc.wantReason != "" && !strings.Contains(decision.FallbackReason, tc.wantReason) {
				t.Fatalf("expected fallback reason containing %q, got %q", tc.wantReason, decision.FallbackReason)
			}
			if tc.wantEnable && decision.FallbackReason != "" {
				t.Fatalf("expected empty fallback reason for enabled decision, got %q", decision.FallbackReason)
			}
		})
	}
}

func TestBootstrapDependencyBlockVolumePlanRunsMissesFromWorkspaceOverlay(t *testing.T) {
	t.Parallel()

	compiled, err := policy.FromProto(testRepositoryTwoDependencyBlocksPolicy())
	if err != nil {
		t.Fatalf("policy.FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(testRepositoryCheckoutProto())
	plan := dependencyBlockVolumePlan{
		Blocks: []dependencyBlockVolumeBlockPlan{
			{
				BlockName: "toolchains",
				Command:   append([]string(nil), compiled.Dependencies.Blocks[0].Command...),
				Env:       cloneDependencyBlockEnv(compiled.Dependencies.Blocks[0].Env),
				Inputs:    append([]string(nil), compiled.Dependencies.Blocks[0].Inputs.Files...),
				CacheHit:  true,
			},
			{
				BlockName: "go-modules",
				Command:   append([]string(nil), compiled.Dependencies.Blocks[1].Command...),
				Env:       cloneDependencyBlockEnv(compiled.Dependencies.Blocks[1].Env),
				Inputs:    append([]string(nil), compiled.Dependencies.Blocks[1].Inputs.Files...),
				Outputs:   compiled.Dependencies.Blocks[1].Outputs,
				CacheKey:  "dependency-volume:go-modules",
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
	publishable, err := svc.bootstrapDependencyBlockVolumePlanInPersistentSandbox(
		context.Background(),
		adapter,
		"cr-test",
		dependencyBlockVolumePublishConfig{},
		compiled,
		backend.FirecrackerConfig{},
		repository,
		plan,
		nil,
	)
	if err != nil {
		t.Fatalf("bootstrapDependencyBlockVolumePlanInPersistentSandbox returned error: %v", err)
	}
	if !publishable {
		t.Fatal("expected dependency block volume plan to remain publishable")
	}
	if got, want := len(gotReqs), 1; got != want {
		t.Fatalf("unexpected run count: got %d want %d", got, want)
	}
	req := gotReqs[0]
	if got, want := strings.Join(req.Command, "\x00"), strings.Join(compiled.Dependencies.Blocks[1].Command, "\x00"); got != want {
		t.Fatalf("unexpected command: got %q want %q", got, want)
	}
	if got, want := req.Dir, "/workspace"; got != want {
		t.Fatalf("unexpected dir: got %q want %q", got, want)
	}
	if !req.ClosedEnv {
		t.Fatal("expected dependency block execution to use a closed environment")
	}
	if !slices.Contains(req.Env, "GOMODCACHE=/root/go/pkg/mod") {
		t.Fatalf("expected GOMODCACHE env, got %v", req.Env)
	}
	if req.InputProjection != nil {
		t.Fatalf("expected dependency block miss to run against the normal workspace, got projection %#v", req.InputProjection)
	}
	if req.OverlayCapture == nil {
		t.Fatal("expected overlay capture request")
	}
	if got, want := req.OverlayCapture.UpperDir, filepath.Join(blockVolumeOverlayCaptureRoot, blockVolumeID(dependencyVolumeStageName, plan.Blocks[1].CacheKey), "upper"); got != want {
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

func TestBootstrapDependencyBlockVolumePlanStopsPublishingAfterEscapedWrites(t *testing.T) {
	compiled, err := policy.FromProto(testRepositoryTwoDependencyBlocksPolicy())
	if err != nil {
		t.Fatalf("policy.FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(testRepositoryCheckoutProto())
	plan := dependencyBlockVolumePlan{
		Blocks: []dependencyBlockVolumeBlockPlan{
			{
				BlockName: "toolchains",
				Command:   append([]string(nil), compiled.Dependencies.Blocks[0].Command...),
				Env:       cloneDependencyBlockEnv(compiled.Dependencies.Blocks[0].Env),
				Inputs:    append([]string(nil), compiled.Dependencies.Blocks[0].Inputs.Files...),
				Outputs:   compiled.Dependencies.Blocks[0].Outputs,
				CacheKey:  "dependency-volume:toolchains",
			},
			{
				BlockName: "go-modules",
				Command:   append([]string(nil), compiled.Dependencies.Blocks[1].Command...),
				Env:       cloneDependencyBlockEnv(compiled.Dependencies.Blocks[1].Env),
				Inputs:    append([]string(nil), compiled.Dependencies.Blocks[1].Inputs.Files...),
				Outputs:   compiled.Dependencies.Blocks[1].Outputs,
				CacheKey:  "dependency-volume:go-modules",
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
					EscapedWrites: []backend.OverlayCaptureEntry{{Path: "/etc/profile", Kind: "write", Mode: 0o644}},
				}
			}
			return result, nil
		},
		snapshotCacheOutputsFn: func(context.Context, backend.SnapshotCacheOutputVolumesRequest) (*backend.SnapshotCacheOutputVolumesResult, error) {
			t.Fatal("did not expect dependency block-volume snapshot after escaped writes")
			return nil, nil
		},
	}
	svc := newTestService(adapter)
	var warnings []string
	publishable, err := svc.bootstrapDependencyBlockVolumePlanInPersistentSandbox(
		context.Background(),
		adapter,
		"cr-test",
		dependencyBlockVolumePublishConfig{Adapter: adapter, Backend: "firecracker", Repository: repository},
		compiled,
		backend.FirecrackerConfig{},
		repository,
		plan,
		CreateSandboxReporterFuncs{OnWarning: func(_ cleanroomv1.CreateSandboxPhase, warning string) {
			warnings = append(warnings, warning)
		}},
	)
	if err != nil {
		t.Fatalf("bootstrapDependencyBlockVolumePlanInPersistentSandbox returned error: %v", err)
	}
	if publishable {
		t.Fatal("expected dependency block volume plan to stop being publishable")
	}
	if got, want := len(gotReqs), 5; got != want {
		t.Fatalf("unexpected run count: got %d want %d", got, want)
	}
	if gotReqs[0].OverlayCapture == nil {
		t.Fatal("expected first dependency block attempt to use overlay capture")
	}
	assertBlockVolumeResetRequest(t, gotReqs[1], policy.NetworkStageDependencies)
	assertBlockVolumeFallbackRequest(t, gotReqs[2], compiled.Dependencies.Blocks[0].Command, policy.NetworkStageDependencies)
	assertBlockVolumeResetRequest(t, gotReqs[3], policy.NetworkStageDependencies)
	assertBlockVolumeFallbackRequest(t, gotReqs[4], compiled.Dependencies.Blocks[1].Command, policy.NetworkStageDependencies)
	if got := adapter.snapshotCacheOutputsCalls; got != 0 {
		t.Fatalf("unexpected cache output snapshot calls: got %d", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "/etc/profile") {
		t.Fatalf("expected escaped-write warning, got %v", warnings)
	}
}

func assertBlockVolumeResetRequest(t *testing.T, req backend.ExecutionRequest, networkStage policy.NetworkStage) {
	t.Helper()
	if got, want := req.NetworkStage, networkStage; got != want {
		t.Fatalf("unexpected reset network stage: got %q want %q", got, want)
	}
	if got, want := strings.Join(req.Command[:min(len(req.Command), 2)], "\x00"), "sh\x00-lc"; got != want {
		t.Fatalf("unexpected reset command prefix: got %v want sh -lc", req.Command)
	}
	if len(req.Command) < 3 || !strings.Contains(req.Command[2], "rm -rf --") {
		t.Fatalf("expected reset command to remove output contents, got %v", req.Command)
	}
	if !req.ClosedEnv {
		t.Fatal("expected reset request to use a closed environment")
	}
	if req.InputProjection != nil {
		t.Fatalf("reset request should not use input projection: %#v", req.InputProjection)
	}
	if len(req.CacheOutputFileCaptures) != 0 {
		t.Fatalf("reset request should not capture output files: %#v", req.CacheOutputFileCaptures)
	}
	if req.OverlayCapture != nil {
		t.Fatalf("reset request should not use overlay capture: %#v", req.OverlayCapture)
	}
}

func assertBlockVolumeFallbackRequest(t *testing.T, req backend.ExecutionRequest, command []string, networkStage policy.NetworkStage) {
	t.Helper()
	if got, want := strings.Join(req.Command, "\x00"), strings.Join(command, "\x00"); got != want {
		t.Fatalf("unexpected fallback command: got %q want %q", got, want)
	}
	if got, want := req.NetworkStage, networkStage; got != want {
		t.Fatalf("unexpected fallback network stage: got %q want %q", got, want)
	}
	if got, want := req.Dir, "/workspace"; got != want {
		t.Fatalf("unexpected fallback dir: got %q want %q", got, want)
	}
	if !req.ClosedEnv {
		t.Fatal("expected fallback request to use a closed environment")
	}
	if req.InputProjection != nil {
		t.Fatalf("fallback request should not use input projection: %#v", req.InputProjection)
	}
	if len(req.CacheOutputFileCaptures) != 0 {
		t.Fatalf("fallback request should not capture output files: %#v", req.CacheOutputFileCaptures)
	}
	if req.OverlayCapture != nil {
		t.Fatalf("fallback request should not use overlay capture: %#v", req.OverlayCapture)
	}
}

func testRepositoryTwoDependencyBlocksPolicy() *cleanroomv1.Policy {
	policyProto := testRepositoryPolicy()
	policyProto.Dependencies = &cleanroomv1.PolicyDependencies{
		Blocks: []*cleanroomv1.PolicyBlock{
			{
				Name:    "toolchains",
				Command: []string{"mise", "install"},
				Inputs:  &cleanroomv1.PolicyBlockInputs{Files: []string{"mise.toml"}},
				Env: map[string]string{
					"MISE_DATA_DIR": "/root/.local/share/mise",
				},
				Outputs: &cleanroomv1.PolicyBlockOutputs{
					Dirs: []string{"/root/.local/share/mise"},
				},
			},
			{
				Name:    "go-modules",
				Command: []string{"mise", "exec", "--", "go", "mod", "download"},
				Inputs:  &cleanroomv1.PolicyBlockInputs{Files: []string{"go.mod", "go.sum"}},
				Env: map[string]string{
					"GOMODCACHE": "/root/go/pkg/mod",
				},
				Outputs: &cleanroomv1.PolicyBlockOutputs{
					Dirs: []string{"/root/go/pkg/mod"},
				},
			},
		},
	}
	return policyProto
}

func dependencyBlockVolumeRuntimeAdapter() *portDialAdapter {
	return &portDialAdapter{capabilities: map[string]bool{
		backend.CapabilitySandboxCacheOutputVolumes:  true,
		backend.CapabilitySandboxOverlayWriteCapture: true,
	}}
}

type recordingCacheStore struct {
	inner   cacheMetadataStore
	lookups []recordingCacheStoreLookup
}

type recordingCacheStoreLookup struct {
	stage    string
	cacheKey string
	hit      bool
}

func (s *recordingCacheStore) Create(ctx context.Context, record cachestore.Record) error {
	return s.inner.Create(ctx, record)
}

func (s *recordingCacheStore) Upsert(ctx context.Context, record cachestore.Record) error {
	return s.inner.Upsert(ctx, record)
}

func (s *recordingCacheStore) GetReady(ctx context.Context, stage, cacheKey string) (cachestore.Record, bool, error) {
	record, ok, err := s.inner.GetReady(ctx, stage, cacheKey)
	s.lookups = append(s.lookups, recordingCacheStoreLookup{
		stage:    stage,
		cacheKey: cacheKey,
		hit:      ok,
	})
	return record, ok, err
}

func (s *recordingCacheStore) Touch(ctx context.Context, stage, cacheKey string) error {
	return s.inner.Touch(ctx, stage, cacheKey)
}

func (s *recordingCacheStore) List(ctx context.Context) ([]cachestore.Record, error) {
	return s.inner.List(ctx)
}

func (s *recordingCacheStore) Delete(ctx context.Context, stage, cacheKey string) error {
	return s.inner.Delete(ctx, stage, cacheKey)
}

func (s *recordingCacheStore) getReadyCount(stage string) int {
	count := 0
	for _, lookup := range s.lookups {
		if lookup.stage == stage {
			count++
		}
	}
	return count
}

func (s *recordingCacheStore) getReadyHitCount(stage string) int {
	count := 0
	for _, lookup := range s.lookups {
		if lookup.stage == stage && lookup.hit {
			count++
		}
	}
	return count
}

func (s *recordingCacheStore) getReadyKeys(stage string) []string {
	keys := make([]string, 0, len(s.lookups))
	for _, lookup := range s.lookups {
		if lookup.stage == stage {
			keys = append(keys, lookup.cacheKey)
		}
	}
	return keys
}

func (s *recordingCacheStore) resetLookups() {
	s.lookups = nil
}

func dependencyBlockVolumeTestRecord(compiled *policy.CompiledPolicy, block dependencyBlockVolumeBlockPlan) cachestore.Record {
	return cachestore.Record{
		CacheKey:                block.CacheKey,
		Stage:                   dependencyVolumeStageName,
		State:                   cacheStateReady,
		BackingSnapshotID:       "dependency-volume-snapshot",
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
