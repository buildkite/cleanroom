package storagegc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/cachestore"
	"github.com/buildkite/cleanroom/internal/imagemgr"
	"github.com/buildkite/cleanroom/internal/snapshotstore"
)

type stubSnapshotStore struct {
	records []snapshotstore.Record
	err     error
}

func (s stubSnapshotStore) List(context.Context) ([]snapshotstore.Record, error) {
	return s.records, s.err
}

type stubCacheStore struct {
	records   []cachestore.Record
	err       error
	deleteErr error
	deleted   []string
}

func (s *stubCacheStore) List(context.Context) ([]cachestore.Record, error) {
	return s.records, s.err
}

func (s *stubCacheStore) Delete(_ context.Context, stage, cacheKey string) error {
	s.deleted = append(s.deleted, stage+"/"+cacheKey)
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return nil
}

type stubImageManager struct {
	records []imagemgr.Record
	err     error
	removed []string
}

func (m *stubImageManager) List(context.Context) ([]imagemgr.Record, error) {
	return m.records, m.err
}

func (m *stubImageManager) Remove(_ context.Context, selector string) ([]imagemgr.Record, error) {
	m.removed = append(m.removed, selector)
	return nil, nil
}

func TestInventoryProtectsReferencesAndFindsOrphans(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpDir, "state-home"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmpDir, "cache-home"))

	stateBase := filepath.Join(tmpDir, "state-home", "cleanroom")
	cacheBase := filepath.Join(tmpDir, "cache-home", "cleanroom")
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	activeSandboxPath := filepath.Join(stateBase, "sandboxes", "active-sandbox", "disk.img")
	writeFile(t, activeSandboxPath, "active")
	orphanSandboxPath := filepath.Join(stateBase, "sandboxes", "orphan-sandbox", "disk.img")
	writeFile(t, orphanSandboxPath, strings.Repeat("o", 7))

	snapshotRootFS := filepath.Join(stateBase, "snapshots", "darwin-vz", "snap-keep", "rootfs.ext4")
	writeFile(t, snapshotRootFS, strings.Repeat("s", 11))
	orphanSnapshotRootFS := filepath.Join(stateBase, "snapshots", "darwin-vz", "snap-orphan", "rootfs.ext4")
	writeFile(t, orphanSnapshotRootFS, strings.Repeat("x", 13))
	stageRootFS := filepath.Join(stateBase, "snapshots", "firecracker", "stage-keep", "rootfs.ext4")
	writeFile(t, stageRootFS, strings.Repeat("c", 17))

	oldExecutionFile := filepath.Join(stateBase, "executions", "exec-old", "stdout")
	writeFile(t, oldExecutionFile, "old")
	oldTime := now.Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Dir(oldExecutionFile), oldTime, oldTime); err != nil {
		t.Fatalf("set old execution mtime: %v", err)
	}
	recentExecutionFile := filepath.Join(stateBase, "executions", "exec-recent", "stdout")
	writeFile(t, recentExecutionFile, "recent")
	if err := os.Chtimes(filepath.Dir(recentExecutionFile), now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatalf("set recent execution mtime: %v", err)
	}

	runtimeRootFS := filepath.Join(cacheBase, "darwin-vz", "runtime-rootfs", "abc.ext4")
	writeFile(t, runtimeRootFS, strings.Repeat("r", 19))
	imageRootFS := filepath.Join(cacheBase, "images", "digest.ext4")
	writeFile(t, imageRootFS, strings.Repeat("i", 23))

	report, err := Inventory(context.Background(), InventoryOptions{
		ActiveSandboxIDs:  []string{"active-sandbox"},
		SandboxStateKnown: true,
		SnapshotStore: stubSnapshotStore{records: []snapshotstore.Record{
			{
				SnapshotID: "snap-keep",
				Backend:    "darwin-vz",
				StorageRef: snapshotRootFS,
				CreatedAt:  now.Add(-3 * time.Hour),
			},
		}},
		CacheStore: &stubCacheStore{records: []cachestore.Record{
			{
				Stage:      "dependencies",
				CacheKey:   "cache-keep",
				Backend:    "firecracker",
				StorageRef: stageRootFS,
				CreatedAt:  now.Add(-4 * time.Hour),
				LastUsedAt: now.Add(-2 * time.Hour),
			},
		}},
		ImageManager: &stubImageManager{records: []imagemgr.Record{
			{
				Digest:     "sha256:digest",
				RootFSPath: imageRootFS,
				SizeBytes:  1,
				CreatedAt:  now.Add(-5 * time.Hour),
				LastUsedAt: now.Add(-time.Hour),
			},
		}},
		Now:             now,
		ExecutionMaxAge: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Inventory returned error: %v", err)
	}

	assertEntry(t, report, KindSandboxRuntime, "active-sandbox", false, "known daemon sandbox")
	assertEntry(t, report, KindSandboxRuntime, "orphan-sandbox", true, "not present in daemon state")
	assertEntry(t, report, KindSnapshot, "snap-keep", false, "explicit snapshot metadata")
	assertEntry(t, report, KindStageCache, "dependencies/cache-keep", false, "stage-cache metadata")
	assertEntry(t, report, KindOrphanSnapshot, "snap-orphan", true, "not referenced by snapshot or cache metadata")
	assertEntry(t, report, KindExecutionArtifact, "exec-old", true, "older than 24h0m0s")
	assertEntry(t, report, KindExecutionArtifact, "exec-recent", false, "recent execution artifacts")
	assertEntry(t, report, KindRuntimeRootFS, "abc", false, "runtime rootfs cache preserved unless selected")
	assertEntry(t, report, KindImageCache, "sha256:digest", false, "image cache metadata")

	expectedOrphanSize, err := pathSize(context.Background(), filepath.Dir(orphanSnapshotRootFS))
	if err != nil {
		t.Fatalf("size orphan snapshot fixture: %v", err)
	}
	if got, want := report.Totals[KindOrphanSnapshot].ReclaimableBytes, expectedOrphanSize; got != want {
		t.Fatalf("unexpected orphan snapshot reclaimable bytes: got %d want %d", got, want)
	}
}

func TestInventoryCountsAllocatedBytesForSparseFiles(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpDir, "state-home"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmpDir, "cache-home"))

	stateBase := filepath.Join(tmpDir, "state-home", "cleanroom")
	cacheBase := filepath.Join(tmpDir, "cache-home", "cleanroom")
	sparseRootFS := filepath.Join(stateBase, "snapshots", "darwin-vz", "snap-orphan", "rootfs.ext4")
	if err := os.MkdirAll(filepath.Dir(sparseRootFS), 0o755); err != nil {
		t.Fatalf("create sparse snapshot directory: %v", err)
	}
	writeSparseFile(t, sparseRootFS, 8<<30)
	sparseRuntimeRootFS := filepath.Join(cacheBase, "darwin-vz", "runtime-rootfs", "runtime.ext4")
	writeSparseFile(t, sparseRuntimeRootFS, 8<<30)

	report, err := Inventory(context.Background(), InventoryOptions{
		SandboxStateKnown: true,
		SnapshotStore:     stubSnapshotStore{},
		CacheStore:        &stubCacheStore{},
		ImageManager:      &stubImageManager{},
	})
	if err != nil {
		t.Fatalf("Inventory returned error: %v", err)
	}
	entry := findEntry(t, report, KindOrphanSnapshot, "snap-orphan")
	if entry.SizeBytes >= 8<<30 {
		t.Fatalf("expected sparse file to report allocated bytes below logical size, got %d", entry.SizeBytes)
	}
	runtimeEntry := findEntry(t, report, KindRuntimeRootFS, "runtime")
	if runtimeEntry.SizeBytes >= 8<<30 {
		t.Fatalf("expected sparse runtime rootfs to report allocated bytes below logical size, got %d", runtimeEntry.SizeBytes)
	}
}

func TestInventoryProtectsSandboxDirsWhenDaemonStateIsUnknown(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpDir, "state-home"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmpDir, "cache-home"))

	stateBase := filepath.Join(tmpDir, "state-home", "cleanroom")
	writeFile(t, filepath.Join(stateBase, "sandboxes", "maybe-active", "disk.img"), "active")

	report, err := Inventory(context.Background(), InventoryOptions{
		SandboxStateKnown: false,
		SnapshotStore:     stubSnapshotStore{},
		CacheStore:        &stubCacheStore{},
		ImageManager:      &stubImageManager{},
	})
	if err != nil {
		t.Fatalf("Inventory returned error: %v", err)
	}
	assertEntry(t, report, KindSandboxRuntime, "maybe-active", false, "daemon sandbox state unavailable")
}

func TestInventoryCanLimitKinds(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpDir, "state-home"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmpDir, "cache-home"))

	stateBase := filepath.Join(tmpDir, "state-home", "cleanroom")
	writeFile(t, filepath.Join(stateBase, "sandboxes", "sandbox-1", "disk.img"), "active")
	writeFile(t, filepath.Join(stateBase, "sandboxes", "sandbox-2", "disk.img"), "active")

	report, err := Inventory(context.Background(), InventoryOptions{
		ActiveSandboxIDs:  []string{"sandbox-1"},
		SandboxRuntimeIDs: []string{"sandbox-1"},
		SandboxStateKnown: true,
		SnapshotStore:     stubSnapshotStore{err: errors.New("snapshot store should not be used")},
		CacheStore:        &stubCacheStore{err: errors.New("cache store should not be used")},
		ImageManager:      &stubImageManager{err: errors.New("image manager should not be used")},
		Kinds:             []string{KindSandboxRuntime},
	})
	if err != nil {
		t.Fatalf("Inventory returned error: %v", err)
	}
	if got, want := len(report.Entries), 1; got != want {
		t.Fatalf("unexpected entry count: got %d want %d", got, want)
	}
	assertEntry(t, report, KindSandboxRuntime, "sandbox-1", false, "known daemon sandbox")
}

func TestInventoryCanSkipSandboxRuntimeSizes(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpDir, "state-home"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmpDir, "cache-home"))

	stateBase := filepath.Join(tmpDir, "state-home", "cleanroom")
	writeFile(t, filepath.Join(stateBase, "sandboxes", "sandbox-1", "disk.img"), "active")

	measured, err := Inventory(context.Background(), InventoryOptions{
		SandboxRuntimeIDs: []string{"sandbox-1"},
		SandboxStateKnown: true,
		Kinds:             []string{KindSandboxRuntime},
	})
	if err != nil {
		t.Fatalf("measured Inventory returned error: %v", err)
	}
	measuredEntry := findEntry(t, measured, KindSandboxRuntime, "sandbox-1")
	if measuredEntry.SizeBytes <= 0 {
		t.Fatalf("expected measured sandbox runtime size to be positive, got %d", measuredEntry.SizeBytes)
	}

	skipped, err := Inventory(context.Background(), InventoryOptions{
		SandboxRuntimeIDs: []string{"sandbox-1"},
		SandboxStateKnown: true,
		Kinds:             []string{KindSandboxRuntime},
		SkipSize:          true,
	})
	if err != nil {
		t.Fatalf("skip-size Inventory returned error: %v", err)
	}
	skippedEntry := findEntry(t, skipped, KindSandboxRuntime, "sandbox-1")
	if skippedEntry.SizeBytes != 0 {
		t.Fatalf("expected skipped sandbox runtime size to be zero, got %d", skippedEntry.SizeBytes)
	}
	if got := skipped.Totals[KindSandboxRuntime].TotalBytes; got != 0 {
		t.Fatalf("expected skipped sandbox runtime total bytes to be zero, got %d", got)
	}
}

func TestInventoryStageCacheFilterKeepsSnapshotProtections(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpDir, "state-home"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmpDir, "cache-home"))

	stateBase := filepath.Join(tmpDir, "state-home", "cleanroom")
	stageRootFS := filepath.Join(stateBase, "snapshots", "firecracker", "stage-shared", "rootfs.ext4")
	writeFile(t, stageRootFS, "cache")
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	report, err := Inventory(context.Background(), InventoryOptions{
		SandboxStateKnown: true,
		SnapshotStore: stubSnapshotStore{records: []snapshotstore.Record{
			{
				SnapshotID: "snap-explicit",
				Backend:    "firecracker",
				StorageRef: stageRootFS,
				CreatedAt:  now,
			},
		}},
		CacheStore: &stubCacheStore{records: []cachestore.Record{
			{
				Stage:      "dependencies",
				CacheKey:   "cache-shared",
				Backend:    "firecracker",
				StorageRef: stageRootFS,
				CreatedAt:  now,
				LastUsedAt: now,
			},
		}},
		ImageManager: &stubImageManager{err: errors.New("image manager should not be used")},
		Kinds:        []string{KindStageCache},
	})
	if err != nil {
		t.Fatalf("Inventory returned error: %v", err)
	}
	if got, want := len(report.Entries), 1; got != want {
		t.Fatalf("unexpected entry count: got %d want %d", got, want)
	}
	entry := findEntry(t, report, KindStageCache, "dependencies/cache-shared")
	if !slices.Contains(entry.ProtectedBy, "snapshot metadata snap-explicit") {
		t.Fatalf("expected snapshot protection, got %#v", entry.ProtectedBy)
	}
}

func TestPlanPruneIncludesAllAndOlderThanCaches(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	report := Report{
		SnapshotRoots: []string{"/tmp/snapshots"},
		Entries: []Entry{
			{
				Kind:       KindRuntimeRootFS,
				ID:         "old-runtime",
				Path:       "/tmp/runtime.ext4",
				SizeBytes:  10,
				LastUsedAt: now.Add(-48 * time.Hour),
			},
			{
				Kind:       KindStageCache,
				ID:         "dependencies/cache",
				Stage:      "dependencies",
				CacheKey:   "cache",
				Backend:    "firecracker",
				Path:       "/tmp/snapshots/firecracker/cache/rootfs.ext4",
				SizeBytes:  20,
				LastUsedAt: now.Add(-48 * time.Hour),
			},
			{
				Kind:        KindStageCache,
				ID:          "dependencies/shared",
				Stage:       "dependencies",
				CacheKey:    "shared",
				Backend:     "firecracker",
				Path:        "/tmp/snapshots/firecracker/shared/rootfs.ext4",
				SizeBytes:   25,
				ProtectedBy: []string{"stage-cache metadata", "snapshot metadata explicit"},
				LastUsedAt:  now.Add(-48 * time.Hour),
			},
			{
				Kind:       KindImageCache,
				ID:         "sha256:image",
				Path:       "/tmp/images/image.ext4",
				SizeBytes:  30,
				LastUsedAt: now.Add(-time.Hour),
			},
			{
				Kind:      KindSnapshot,
				ID:        "explicit",
				Path:      "/tmp/snapshots/firecracker/explicit/rootfs.ext4",
				SizeBytes: 40,
			},
		},
	}

	agedPlan := PlanPrune(report, PruneOptions{OlderThan: 24 * time.Hour, Now: now})
	if got, want := len(agedPlan.Actions), 2; got != want {
		t.Fatalf("unexpected aged action count: got %d want %d", got, want)
	}
	assertPlanAction(t, agedPlan, KindRuntimeRootFS, "old-runtime")
	assertPlanAction(t, agedPlan, KindStageCache, "dependencies/cache")

	allPlan := PlanPrune(report, PruneOptions{All: true, Now: now})
	if got, want := len(allPlan.Actions), 3; got != want {
		t.Fatalf("unexpected --all action count: got %d want %d", got, want)
	}
	assertPlanAction(t, allPlan, KindRuntimeRootFS, "old-runtime")
	assertPlanAction(t, allPlan, KindStageCache, "dependencies/cache")
	assertPlanAction(t, allPlan, KindImageCache, "sha256:image")
}

func TestPlanLifecyclePruneDeletesTerminatedSandboxRuntime(t *testing.T) {
	tmpDir := t.TempDir()
	runtimeDir := filepath.Join(tmpDir, "state", "cleanroom", "sandboxes", "sandbox-1")
	writeFile(t, filepath.Join(runtimeDir, "rootfs.ext4"), "sandbox")
	cacheDir := filepath.Join(tmpDir, "cache", "cleanroom")

	report := Report{
		StateBaseDir:      filepath.Join(tmpDir, "state", "cleanroom"),
		CacheBaseDir:      cacheDir,
		SandboxStateKnown: true,
		Entries: []Entry{
			{
				Kind:        KindSandboxRuntime,
				ID:          "sandbox-1",
				Path:        runtimeDir,
				SizeBytes:   7,
				Reason:      "known daemon sandbox",
				ProtectedBy: []string{"daemon state"},
			},
		},
	}
	plan := PlanLifecyclePrune(report, LifecycleOptions{TerminatedSandboxIDs: []string{"sandbox-1"}})
	if got, want := len(plan.Actions), 1; got != want {
		t.Fatalf("unexpected lifecycle action count: got %d want %d", got, want)
	}
	if got, want := plan.Actions[0].Reason, "terminated sandbox lifecycle cleanup"; got != want {
		t.Fatalf("unexpected action reason: got %q want %q", got, want)
	}
	result, err := ExecutePrune(context.Background(), report, plan, ExecuteOptions{
		CacheStore:   &stubCacheStore{err: errors.New("cache store should not be used")},
		ImageManager: &stubImageManager{err: errors.New("image manager should not be used")},
	})
	if err != nil {
		t.Fatalf("ExecutePrune returned error: %v", err)
	}
	if got, want := result.DeletedEntries, 1; got != want {
		t.Fatalf("unexpected deleted entries: got %d want %d", got, want)
	}
	if _, err := os.Stat(runtimeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected runtime dir to be removed, stat err: %v", err)
	}
}

func TestPlanPruneSkipsStageCacheOutsideManagedSnapshotStorage(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	report := Report{
		SnapshotRoots: []string{"/tmp/snapshots"},
		Entries: []Entry{
			{
				Kind:       KindStageCache,
				ID:         "dependencies/repo",
				Stage:      "dependencies",
				CacheKey:   "repo",
				Backend:    "firecracker",
				Path:       "/tmp/state/cleanroom/repos/rootfs.ext4",
				SizeBytes:  20,
				LastUsedAt: now.Add(-48 * time.Hour),
			},
			{
				Kind:       KindStageCache,
				ID:         "dependencies/nested",
				Stage:      "dependencies",
				CacheKey:   "nested",
				Backend:    "firecracker",
				Path:       "/tmp/snapshots/firecracker/cache/nested/rootfs.ext4",
				SizeBytes:  20,
				LastUsedAt: now.Add(-48 * time.Hour),
			},
			{
				Kind:       KindStageCache,
				ID:         "dependencies/wrong-backend",
				Stage:      "dependencies",
				CacheKey:   "wrong-backend",
				Backend:    "darwin-vz",
				Path:       "/tmp/snapshots/firecracker/cache/rootfs.ext4",
				SizeBytes:  20,
				LastUsedAt: now.Add(-48 * time.Hour),
			},
		},
	}

	allPlan := PlanPrune(report, PruneOptions{All: true, Now: now})
	if got := len(allPlan.Actions); got != 0 {
		t.Fatalf("expected no unmanaged stage-cache actions, got %#v", allPlan.Actions)
	}
}

func TestExecutePruneDeletesOwnedPathsAndStageCacheMetadata(t *testing.T) {
	tmpDir := t.TempDir()
	stageRootFS := filepath.Join(tmpDir, "state", "cleanroom", "snapshots", "firecracker", "cache", "rootfs.ext4")
	writeFile(t, stageRootFS, "cache")
	report := Report{
		StateBaseDir:  filepath.Join(tmpDir, "state", "cleanroom"),
		CacheBaseDir:  filepath.Join(tmpDir, "cache", "cleanroom"),
		SnapshotRoots: []string{filepath.Join(tmpDir, "state", "cleanroom", "snapshots")},
		Entries: []Entry{
			{
				Kind:       KindStageCache,
				ID:         "dependencies/cache",
				Stage:      "dependencies",
				CacheKey:   "cache",
				Backend:    "firecracker",
				Path:       stageRootFS,
				SizeBytes:  5,
				LastUsedAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			},
		},
	}
	plan := PlanPrune(report, PruneOptions{All: true, Now: time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)})
	cacheStore := &stubCacheStore{}
	result, err := ExecutePrune(context.Background(), report, plan, ExecuteOptions{CacheStore: cacheStore, ImageManager: &stubImageManager{}})
	if err != nil {
		t.Fatalf("ExecutePrune returned error: %v", err)
	}
	if got, want := result.DeletedEntries, 1; got != want {
		t.Fatalf("unexpected deleted entries: got %d want %d", got, want)
	}
	if _, err := os.Stat(filepath.Dir(stageRootFS)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stage cache directory to be removed, stat err=%v", err)
	}
	if got, want := cacheStore.deleted, []string{"dependencies/cache"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("unexpected metadata deletes: got %v want %v", got, want)
	}
}

func TestExecutePruneKeepsStageCacheStorageWhenMetadataDeleteFails(t *testing.T) {
	tmpDir := t.TempDir()
	stageRootFS := filepath.Join(tmpDir, "state", "cleanroom", "snapshots", "firecracker", "cache", "rootfs.ext4")
	writeFile(t, stageRootFS, "cache")
	report := Report{
		StateBaseDir:  filepath.Join(tmpDir, "state", "cleanroom"),
		CacheBaseDir:  filepath.Join(tmpDir, "cache", "cleanroom"),
		SnapshotRoots: []string{filepath.Join(tmpDir, "state", "cleanroom", "snapshots")},
		Entries: []Entry{
			{
				Kind:       KindStageCache,
				ID:         "dependencies/cache",
				Stage:      "dependencies",
				CacheKey:   "cache",
				Backend:    "firecracker",
				Path:       stageRootFS,
				StorageRef: stageRootFS,
				SizeBytes:  5,
				LastUsedAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			},
		},
	}
	plan := PlanPrune(report, PruneOptions{All: true, Now: time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)})
	cacheStore := &stubCacheStore{deleteErr: errors.New("metadata locked")}
	if _, err := ExecutePrune(context.Background(), report, plan, ExecuteOptions{CacheStore: cacheStore, ImageManager: &stubImageManager{}}); err == nil {
		t.Fatal("expected ExecutePrune to return metadata delete error")
	}
	if _, err := os.Stat(filepath.Dir(stageRootFS)); err != nil {
		t.Fatalf("expected stage cache directory to remain, stat err=%v", err)
	}
}

func TestExecutePruneRefusesPathsOutsideCleanroomRoots(t *testing.T) {
	tmpDir := t.TempDir()
	report := Report{
		StateBaseDir:  filepath.Join(tmpDir, "state", "cleanroom"),
		CacheBaseDir:  filepath.Join(tmpDir, "cache", "cleanroom"),
		SnapshotRoots: []string{filepath.Join(tmpDir, "state", "cleanroom", "snapshots")},
	}
	plan := Plan{
		Actions: []Action{{
			Kind: KindOrphanSnapshot,
			ID:   "outside",
			Path: filepath.Join(tmpDir, "outside"),
		}},
	}
	if _, err := ExecutePrune(context.Background(), report, plan, ExecuteOptions{CacheStore: &stubCacheStore{}, ImageManager: &stubImageManager{}}); err == nil {
		t.Fatal("expected ExecutePrune to refuse an outside path")
	}
}

func assertEntry(t *testing.T, report Report, kind, id string, reclaimable bool, reason string) {
	t.Helper()
	entry := findEntry(t, report, kind, id)
	if entry.Reclaimable != reclaimable {
		t.Fatalf("entry %s/%s reclaimable got %t want %t", kind, id, entry.Reclaimable, reclaimable)
	}
	if !strings.Contains(entry.Reason, reason) {
		t.Fatalf("entry %s/%s reason got %q want to contain %q", kind, id, entry.Reason, reason)
	}
}

func findEntry(t *testing.T, report Report, kind, id string) Entry {
	t.Helper()
	for _, entry := range report.Entries {
		if entry.Kind == kind && entry.ID == id {
			return entry
		}
	}
	t.Fatalf("entry %s/%s not found in %#v", kind, id, report.Entries)
	return Entry{}
}

func assertPlanAction(t *testing.T, plan Plan, kind, id string) {
	t.Helper()
	for _, action := range plan.Actions {
		if action.Kind == kind && action.ID == id {
			return
		}
	}
	t.Fatalf("action %s/%s not found in %#v", kind, id, plan.Actions)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeSparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", path, err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create sparse file %s: %v", path, err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		t.Fatalf("truncate sparse file %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close sparse file %s: %v", path, err)
	}
}
