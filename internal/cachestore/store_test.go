package cachestore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

func TestStoreCreateGetReadyListDelete(t *testing.T) {
	t.Parallel()

	store, err := New(Options{MetadataDBPath: filepath.Join(t.TempDir(), "caches.db")})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	record := Record{
		CacheKey:   "workspace-seed:test",
		Stage:      "workspace",
		State:      "ready",
		Backend:    "firecracker",
		PolicyHash: "policy-hash",
		Policy: &cleanroomv1.Policy{
			Version:        1,
			ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			NetworkDefault: "deny",
			Hash:           "policy-hash",
		},
		Repository: &cleanroomv1.RepositoryCheckout{
			RemoteUrl:      "https://github.com/buildkite/cleanroom.git",
			CommitSha:      "0123456789abcdef0123456789abcdef01234567",
			DestinationDir: "/workspace",
			Submodules:     true,
			Branch:         "main",
		},
		ParentCacheKey:      "runtime:test",
		StorageRef:          "/tmp/workspace-test.ext4",
		StorageDriver:       "file",
		InputManifestDigest: "sha256:manifest",
		CreatedAt:           time.Unix(1700000000, 123).UTC(),
		LastUsedAt:          time.Unix(1700000001, 456).UTC(),
		ProducerVersion:     "cleanroom-test/1",
	}
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	got, ok, err := store.GetReady(context.Background(), record.Stage, record.CacheKey)
	if err != nil {
		t.Fatalf("GetReady returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected stored ready cache")
	}
	if got.CacheKey != record.CacheKey || got.Stage != record.Stage || got.State != record.State || got.PolicyHash != record.PolicyHash || got.StorageRef != record.StorageRef || got.StorageDriver != record.StorageDriver || got.ParentCacheKey != record.ParentCacheKey || got.InputManifestDigest != record.InputManifestDigest || got.ProducerVersion != record.ProducerVersion {
		t.Fatalf("unexpected cache record: %#v", got)
	}
	if !got.CreatedAt.Equal(record.CreatedAt) {
		t.Fatalf("unexpected created_at: got %s want %s", got.CreatedAt.Format(time.RFC3339Nano), record.CreatedAt.Format(time.RFC3339Nano))
	}
	if !got.LastUsedAt.Equal(record.LastUsedAt) {
		t.Fatalf("unexpected last_used_at: got %s want %s", got.LastUsedAt.Format(time.RFC3339Nano), record.LastUsedAt.Format(time.RFC3339Nano))
	}
	if got.Policy == nil || got.Policy.GetImageRef() != record.Policy.GetImageRef() {
		t.Fatalf("unexpected stored policy: %#v", got.Policy)
	}
	if got.Repository == nil || got.Repository.GetDestinationDir() != record.Repository.GetDestinationDir() || got.Repository.GetCommitSha() != record.Repository.GetCommitSha() {
		t.Fatalf("unexpected stored repository: %#v", got.Repository)
	}

	items, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got, want := len(items), 1; got != want {
		t.Fatalf("unexpected cache count: got %d want %d", got, want)
	}

	if err := store.Delete(context.Background(), record.Stage, record.CacheKey); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if _, ok, err := store.GetReady(context.Background(), record.Stage, record.CacheKey); err != nil {
		t.Fatalf("GetReady after delete returned error: %v", err)
	} else if ok {
		t.Fatal("expected cache to be deleted")
	}
}

func TestStoreGetReadyFiltersNonReadyStates(t *testing.T) {
	t.Parallel()

	store, err := New(Options{MetadataDBPath: filepath.Join(t.TempDir(), "caches.db")})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	record := Record{
		CacheKey:        "dependency-seed:test",
		Stage:           "dependency",
		State:           "failed",
		Backend:         "firecracker",
		PolicyHash:      "policy-hash",
		Policy:          testPolicy(),
		StorageRef:      "/tmp/dependency-test.ext4",
		StorageDriver:   "file",
		ProducerVersion: "cleanroom-test/1",
	}
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	if _, ok, err := store.GetReady(context.Background(), record.Stage, record.CacheKey); err != nil {
		t.Fatalf("GetReady returned error: %v", err)
	} else if ok {
		t.Fatal("expected non-ready cache to be hidden from GetReady")
	}
}

func TestStoreUpdateLastUsedAtAndTouch(t *testing.T) {
	t.Parallel()

	store, err := New(Options{MetadataDBPath: filepath.Join(t.TempDir(), "caches.db")})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	record := Record{
		CacheKey:        "runtime:test",
		Stage:           "runtime",
		State:           "ready",
		Backend:         "firecracker",
		PolicyHash:      "policy-hash",
		Policy:          testPolicy(),
		StorageRef:      "/tmp/runtime-test.ext4",
		StorageDriver:   "file",
		ProducerVersion: "cleanroom-test/1",
		CreatedAt:       time.Unix(1700000000, 0).UTC(),
		LastUsedAt:      time.Unix(1700000000, 0).UTC(),
	}
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	next := time.Unix(1700000100, 789).UTC()
	if err := store.UpdateLastUsedAt(context.Background(), record.Stage, record.CacheKey, next); err != nil {
		t.Fatalf("UpdateLastUsedAt returned error: %v", err)
	}

	got, ok, err := store.GetReady(context.Background(), record.Stage, record.CacheKey)
	if err != nil {
		t.Fatalf("GetReady returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected stored cache")
	}
	if !got.LastUsedAt.Equal(next) {
		t.Fatalf("unexpected last_used_at after update: got %s want %s", got.LastUsedAt.Format(time.RFC3339Nano), next.Format(time.RFC3339Nano))
	}

	if err := store.Touch(context.Background(), record.Stage, record.CacheKey); err != nil {
		t.Fatalf("Touch returned error: %v", err)
	}
	touched, ok, err := store.GetReady(context.Background(), record.Stage, record.CacheKey)
	if err != nil {
		t.Fatalf("GetReady after touch returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected stored cache after touch")
	}
	if !touched.LastUsedAt.After(next) {
		t.Fatalf("expected touched last_used_at to move forward, got %s want after %s", touched.LastUsedAt.Format(time.RFC3339Nano), next.Format(time.RFC3339Nano))
	}
}

func TestStoreListOrdersByCreatedAtNanoseconds(t *testing.T) {
	t.Parallel()

	store, err := New(Options{MetadataDBPath: filepath.Join(t.TempDir(), "caches.db")})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	first := Record{
		CacheKey:        "workspace-seed:first",
		Stage:           "workspace",
		State:           "ready",
		Backend:         "firecracker",
		PolicyHash:      "policy-hash",
		Policy:          testPolicy(),
		StorageRef:      "/tmp/workspace-first.ext4",
		StorageDriver:   "file",
		CreatedAt:       time.Unix(1700000000, 100).UTC(),
		LastUsedAt:      time.Unix(1700000000, 100).UTC(),
		ProducerVersion: "cleanroom-test/1",
	}
	second := Record{
		CacheKey:        "workspace-seed:second",
		Stage:           "workspace",
		State:           "ready",
		Backend:         "firecracker",
		PolicyHash:      "policy-hash",
		Policy:          testPolicy(),
		StorageRef:      "/tmp/workspace-second.ext4",
		StorageDriver:   "file",
		CreatedAt:       time.Unix(1700000000, 200).UTC(),
		LastUsedAt:      time.Unix(1700000000, 200).UTC(),
		ProducerVersion: "cleanroom-test/1",
	}
	if err := store.Create(context.Background(), second); err != nil {
		t.Fatalf("Create second returned error: %v", err)
	}
	if err := store.Create(context.Background(), first); err != nil {
		t.Fatalf("Create first returned error: %v", err)
	}

	items, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got, want := len(items), 2; got != want {
		t.Fatalf("unexpected cache count: got %d want %d", got, want)
	}
	if got, want := items[0].CacheKey, first.CacheKey; got != want {
		t.Fatalf("unexpected first cache in list: got %q want %q", got, want)
	}
	if got, want := items[1].CacheKey, second.CacheKey; got != want {
		t.Fatalf("unexpected second cache in list: got %q want %q", got, want)
	}
}

func TestNewMigratesLegacySchemalessDatabase(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "caches.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy cache db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy cache db: %v", err)
	}

	store, err := New(Options{MetadataDBPath: dbPath})
	if err != nil {
		t.Fatalf("New returned error for legacy cache db: %v", err)
	}

	record := Record{
		CacheKey:        "runtime:test",
		Stage:           "runtime",
		State:           "ready",
		Backend:         "firecracker",
		PolicyHash:      "policy-hash",
		Policy:          testPolicy(),
		StorageRef:      "/tmp/runtime-test.ext4",
		StorageDriver:   "file",
		CreatedAt:       time.Unix(1700000000, 123).UTC(),
		LastUsedAt:      time.Unix(1700000000, 456).UTC(),
		ProducerVersion: "cleanroom-test/1",
	}
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatalf("Create returned error after legacy init: %v", err)
	}
}

func testPolicy() *cleanroomv1.Policy {
	return &cleanroomv1.Policy{
		Version:        1,
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
		Hash:           "policy-hash",
	}
}
