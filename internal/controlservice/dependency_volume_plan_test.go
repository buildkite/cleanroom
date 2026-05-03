package controlservice

import (
	"context"
	"os"
	"path/filepath"
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
			if got, want := adapter.runCalls, 2; got != want {
				t.Fatalf("expected aggregate repository + dependency bootstrap executions, got %d want %d", got, want)
			}
		})
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
