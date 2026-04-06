package snapshotstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

func TestStoreCreateGetListDelete(t *testing.T) {
	t.Parallel()

	store, err := New(Options{MetadataDBPath: filepath.Join(t.TempDir(), "snapshots.db")})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	record := Record{
		SnapshotID:      "snap-test",
		SourceSandboxID: "cr-test",
		Backend:         "firecracker",
		Name:            "golden",
		PolicyHash:      "policy-hash",
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
		StorageRef:    "/tmp/snap-test.ext4",
		StorageDriver: "file",
		CreatedAt:     time.Unix(1700000000, 0).UTC(),
	}
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	got, ok, err := store.Get(context.Background(), "snap-test")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected stored snapshot")
	}
	if got.SnapshotID != record.SnapshotID || got.PolicyHash != record.PolicyHash || got.StorageRef != record.StorageRef || got.StorageDriver != record.StorageDriver {
		t.Fatalf("unexpected snapshot record: %#v", got)
	}
	if !got.CreatedAt.Equal(record.CreatedAt) {
		t.Fatalf("unexpected created_at: got %s want %s", got.CreatedAt.Format(time.RFC3339Nano), record.CreatedAt.Format(time.RFC3339Nano))
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
		t.Fatalf("unexpected snapshot count: got %d want %d", got, want)
	}

	if err := store.Delete(context.Background(), "snap-test"); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if _, ok, err := store.Get(context.Background(), "snap-test"); err != nil {
		t.Fatalf("Get after delete returned error: %v", err)
	} else if ok {
		t.Fatal("expected snapshot to be deleted")
	}
}

func TestStoreListOrdersByCreatedAtNanoseconds(t *testing.T) {
	t.Parallel()

	store, err := New(Options{MetadataDBPath: filepath.Join(t.TempDir(), "snapshots.db")})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	basePolicy := &cleanroomv1.Policy{
		Version:        1,
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
		Hash:           "policy-hash",
	}
	baseRepository := &cleanroomv1.RepositoryCheckout{
		RemoteUrl:      "https://github.com/buildkite/cleanroom.git",
		CommitSha:      "0123456789abcdef0123456789abcdef01234567",
		DestinationDir: "/workspace",
	}

	first := Record{
		SnapshotID:      "snap-first",
		SourceSandboxID: "cr-first",
		Backend:         "firecracker",
		Name:            "workspace-seed:first",
		PolicyHash:      "policy-hash",
		Policy:          basePolicy,
		Repository:      baseRepository,
		StorageRef:      "/tmp/snap-first.ext4",
		StorageDriver:   "file",
		CreatedAt:       time.Unix(1700000000, 100).UTC(),
	}
	second := Record{
		SnapshotID:      "snap-second",
		SourceSandboxID: "cr-second",
		Backend:         "firecracker",
		Name:            "workspace-seed:second",
		PolicyHash:      "policy-hash",
		Policy:          basePolicy,
		Repository:      baseRepository,
		StorageRef:      "/tmp/snap-second.ext4",
		StorageDriver:   "file",
		CreatedAt:       time.Unix(1700000000, 200).UTC(),
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
		t.Fatalf("unexpected snapshot count: got %d want %d", got, want)
	}
	if got, want := items[0].SnapshotID, first.SnapshotID; got != want {
		t.Fatalf("unexpected first snapshot in list: got %q want %q", got, want)
	}
	if got, want := items[1].SnapshotID, second.SnapshotID; got != want {
		t.Fatalf("unexpected second snapshot in list: got %q want %q", got, want)
	}
}

func TestNewMigratesLegacyStoreBeforeCreatingNanoIndex(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "snapshots.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy snapshot db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})

	if _, err := db.Exec(`
		CREATE TABLE snapshots (
			snapshot_id TEXT PRIMARY KEY,
			source_sandbox_id TEXT NOT NULL,
			backend TEXT NOT NULL,
			name TEXT NOT NULL,
			policy_hash TEXT NOT NULL,
			policy_proto BLOB NOT NULL,
			repository_proto BLOB,
			storage_driver TEXT NOT NULL DEFAULT 'file',
			storage_ref TEXT NOT NULL,
			created_at_unix INTEGER NOT NULL
		);
		CREATE INDEX idx_snapshots_created_at ON snapshots(created_at_unix);
	`); err != nil {
		t.Fatalf("create legacy snapshot schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy snapshot db: %v", err)
	}

	store, err := New(Options{MetadataDBPath: dbPath})
	if err != nil {
		t.Fatalf("New returned error for legacy snapshot db: %v", err)
	}

	record := Record{
		SnapshotID:      "snap-test",
		SourceSandboxID: "cr-test",
		Backend:         "firecracker",
		Name:            "golden",
		PolicyHash:      "policy-hash",
		Policy: &cleanroomv1.Policy{
			Version:        1,
			ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			NetworkDefault: "deny",
			Hash:           "policy-hash",
		},
		StorageRef:    "/tmp/snap-test.ext4",
		StorageDriver: "file",
		CreatedAt:     time.Unix(1700000000, 123).UTC(),
	}
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatalf("Create returned error after legacy migration: %v", err)
	}

	got, ok, err := store.Get(context.Background(), record.SnapshotID)
	if err != nil {
		t.Fatalf("Get returned error after legacy migration: %v", err)
	}
	if !ok {
		t.Fatal("expected migrated legacy store to return stored snapshot")
	}
	if !got.CreatedAt.Equal(record.CreatedAt) {
		t.Fatalf("unexpected created_at after legacy migration: got %s want %s", got.CreatedAt.Format(time.RFC3339Nano), record.CreatedAt.Format(time.RFC3339Nano))
	}
}
