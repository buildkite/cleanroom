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
		CacheKey:          "workspace-stage:test",
		Stage:             "workspace",
		State:             "ready",
		BackingSnapshotID: "snapshot-workspace-test",
		Backend:           "firecracker",
		Architecture:      "arm64",
		RuntimeBaseKey:    "runtime-base:test",
		PolicyHash:        "policy-hash",
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
		RepositoryHasChangeset:   true,
		RepositoryChangesetID:    "changeset:v1:test",
		ParentCacheKey:           "runtime:test",
		ReuseMode:                "portable",
		StorageRef:               "/tmp/workspace-test.ext4",
		StorageDriver:            "file",
		StorageSizeBytes:         12345,
		ExclusiveSizeBytes:       2345,
		DriverMetadata:           `{"version":1,"driver":"file"}`,
		InputManifestDigest:      "sha256:manifest",
		CommandDigest:            "sha256:command",
		EnvDigest:                "sha256:env",
		NormalizedOutputsDigest:  "sha256:outputs",
		OutputManifestDigest:     "sha256:output-manifest",
		DependencyKeyFilesDigest: "sha256:dependency-key-files",
		OutputRecords: []OutputRecord{
			{
				Kind:           "directory",
				Path:           "/root/go/pkg/mod",
				VolumeSubpath:  "dirs/root-go-pkg-mod",
				StorageDriver:  "file",
				StorageRef:     "/tmp/go-mod-output.ext4",
				SnapshotRef:    "/tmp/go-mod-output-snapshot.ext4",
				ManifestDigest: "sha256:go-mod-output",
			},
		},
		CheckoutRefreshRequired: true,
		ImportedFromPeer:        true,
		CreatedAt:               time.Unix(1700000000, 123).UTC(),
		LastUsedAt:              time.Unix(1700000001, 456).UTC(),
		LastValidatedAt:         time.Unix(1700000002, 789).UTC(),
		ProducerVersion:         "cleanroom-test/1",
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
	if got.CacheKey != record.CacheKey || got.Stage != record.Stage || got.State != record.State || got.PolicyHash != record.PolicyHash || got.StorageRef != record.StorageRef || got.StorageDriver != record.StorageDriver || got.StorageSizeBytes != record.StorageSizeBytes || got.ExclusiveSizeBytes != record.ExclusiveSizeBytes || got.DriverMetadata != record.DriverMetadata || got.Architecture != record.Architecture || got.RuntimeBaseKey != record.RuntimeBaseKey || got.ParentCacheKey != record.ParentCacheKey || got.ReuseMode != record.ReuseMode || got.RepositoryChangesetID != record.RepositoryChangesetID || got.InputManifestDigest != record.InputManifestDigest || got.CommandDigest != record.CommandDigest || got.EnvDigest != record.EnvDigest || got.NormalizedOutputsDigest != record.NormalizedOutputsDigest || got.OutputManifestDigest != record.OutputManifestDigest || got.DependencyKeyFilesDigest != record.DependencyKeyFilesDigest || got.CheckoutRefreshRequired != record.CheckoutRefreshRequired || got.ImportedFromPeer != record.ImportedFromPeer || got.ProducerVersion != record.ProducerVersion {
		t.Fatalf("unexpected cache record: %#v", got)
	}
	if got, want := len(got.OutputRecords), len(record.OutputRecords); got != want {
		t.Fatalf("unexpected output record count: got %d want %d", got, want)
	}
	if got, want := got.OutputRecords[0], record.OutputRecords[0]; got != want {
		t.Fatalf("unexpected output record: %#v want %#v", got, want)
	}
	if !got.CreatedAt.Equal(record.CreatedAt) {
		t.Fatalf("unexpected created_at: got %s want %s", got.CreatedAt.Format(time.RFC3339Nano), record.CreatedAt.Format(time.RFC3339Nano))
	}
	if !got.LastUsedAt.Equal(record.LastUsedAt) {
		t.Fatalf("unexpected last_used_at: got %s want %s", got.LastUsedAt.Format(time.RFC3339Nano), record.LastUsedAt.Format(time.RFC3339Nano))
	}
	if !got.LastValidatedAt.Equal(record.LastValidatedAt) {
		t.Fatalf("unexpected last_validated_at: got %s want %s", got.LastValidatedAt.Format(time.RFC3339Nano), record.LastValidatedAt.Format(time.RFC3339Nano))
	}
	if got.Policy == nil || got.Policy.GetImageRef() != record.Policy.GetImageRef() {
		t.Fatalf("unexpected stored policy: %#v", got.Policy)
	}
	if got.Repository == nil || got.Repository.GetDestinationDir() != record.Repository.GetDestinationDir() || got.Repository.GetCommitSha() != record.Repository.GetCommitSha() {
		t.Fatalf("unexpected stored repository: %#v", got.Repository)
	}
	if got.RepositoryHasChangeset != record.RepositoryHasChangeset {
		t.Fatalf("unexpected repository_has_changeset: got %v want %v", got.RepositoryHasChangeset, record.RepositoryHasChangeset)
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
		CacheKey:          "dependency-stage:test",
		Stage:             "dependency",
		State:             "failed",
		BackingSnapshotID: "snapshot-dependency-test",
		Backend:           "firecracker",
		PolicyHash:        "policy-hash",
		Policy:            testPolicy(),
		StorageRef:        "/tmp/dependency-test.ext4",
		StorageDriver:     "file",
		ProducerVersion:   "cleanroom-test/1",
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
		CacheKey:          "runtime:test",
		Stage:             "runtime",
		State:             "ready",
		BackingSnapshotID: "snapshot-runtime-test",
		Backend:           "firecracker",
		PolicyHash:        "policy-hash",
		Policy:            testPolicy(),
		StorageRef:        "/tmp/runtime-test.ext4",
		StorageDriver:     "file",
		ProducerVersion:   "cleanroom-test/1",
		CreatedAt:         time.Unix(1700000000, 0).UTC(),
		LastUsedAt:        time.Unix(1700000000, 0).UTC(),
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
		CacheKey:          "workspace-stage:first",
		Stage:             "workspace",
		State:             "ready",
		BackingSnapshotID: "snapshot-workspace-first",
		Backend:           "firecracker",
		PolicyHash:        "policy-hash",
		Policy:            testPolicy(),
		StorageRef:        "/tmp/workspace-first.ext4",
		StorageDriver:     "file",
		CreatedAt:         time.Unix(1700000000, 100).UTC(),
		LastUsedAt:        time.Unix(1700000000, 100).UTC(),
		ProducerVersion:   "cleanroom-test/1",
	}
	second := Record{
		CacheKey:          "workspace-stage:second",
		Stage:             "workspace",
		State:             "ready",
		BackingSnapshotID: "snapshot-workspace-second",
		Backend:           "firecracker",
		PolicyHash:        "policy-hash",
		Policy:            testPolicy(),
		StorageRef:        "/tmp/workspace-second.ext4",
		StorageDriver:     "file",
		CreatedAt:         time.Unix(1700000000, 200).UTC(),
		LastUsedAt:        time.Unix(1700000000, 200).UTC(),
		ProducerVersion:   "cleanroom-test/1",
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

func TestStoreCreateRejectsDuplicateStageCacheKey(t *testing.T) {
	t.Parallel()

	store, err := New(Options{MetadataDBPath: filepath.Join(t.TempDir(), "caches.db")})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	record := Record{
		CacheKey:          "workspace-stage:test",
		Stage:             "workspace",
		State:             "ready",
		BackingSnapshotID: "snapshot-workspace-test",
		Backend:           "firecracker",
		PolicyHash:        "policy-hash",
		Policy:            testPolicy(),
		StorageRef:        "/tmp/workspace-test.ext4",
		StorageDriver:     "file",
		ProducerVersion:   "cleanroom-test/1",
	}
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatalf("first Create returned error: %v", err)
	}
	if err := store.Create(context.Background(), record); err == nil {
		t.Fatal("expected duplicate cache insert to fail")
	}

	items, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got, want := len(items), 1; got != want {
		t.Fatalf("expected duplicate cache insert to keep one record, got %d want %d", got, want)
	}
}

func TestStorePartitionsReadyRecordsByOwner(t *testing.T) {
	t.Parallel()

	store, err := New(Options{MetadataDBPath: filepath.Join(t.TempDir(), "caches.db")})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	record := Record{
		CacheKey:          "workspace-stage:test",
		Stage:             "workspace",
		State:             "ready",
		BackingSnapshotID: "snapshot-workspace-test",
		Backend:           "firecracker",
		PolicyHash:        "policy-hash",
		Policy:            testPolicy(),
		StorageRef:        "/tmp/workspace-test.ext4",
		StorageDriver:     "file",
		ProducerVersion:   "cleanroom-test/1",
	}
	alice := record
	alice.OwnerPrincipalID = "oidc:test:alice"
	alice.OwnerScope = "scope:alice"
	alice.BackingSnapshotID = "snapshot-alice"
	alice.StorageRef = "/tmp/alice.ext4"
	bob := record
	bob.OwnerPrincipalID = "oidc:test:bob"
	bob.OwnerScope = "scope:bob"
	bob.BackingSnapshotID = "snapshot-bob"
	bob.StorageRef = "/tmp/bob.ext4"
	if err := store.Create(context.Background(), alice); err != nil {
		t.Fatalf("Create alice returned error: %v", err)
	}
	if err := store.Create(context.Background(), bob); err != nil {
		t.Fatalf("Create bob with same stage/cache_key returned error: %v", err)
	}

	if _, ok, err := store.GetReady(context.Background(), record.Stage, record.CacheKey); err != nil {
		t.Fatalf("ownerless GetReady returned error: %v", err)
	} else if ok {
		t.Fatal("ownerless GetReady returned an owned record")
	}
	gotAlice, ok, err := store.GetReadyForOwner(context.Background(), record.Stage, record.CacheKey, "oidc:test:alice")
	if err != nil {
		t.Fatalf("GetReadyForOwner alice returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected alice-owned record")
	}
	if gotAlice.StorageRef != alice.StorageRef || gotAlice.OwnerScope != alice.OwnerScope {
		t.Fatalf("unexpected alice record: %#v", gotAlice)
	}
	gotBob, ok, err := store.GetReadyForOwner(context.Background(), record.Stage, record.CacheKey, "oidc:test:bob")
	if err != nil {
		t.Fatalf("GetReadyForOwner bob returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected bob-owned record")
	}
	if gotBob.StorageRef != bob.StorageRef || gotBob.OwnerScope != bob.OwnerScope {
		t.Fatalf("unexpected bob record: %#v", gotBob)
	}

	next := time.Unix(1700000300, 0).UTC()
	if err := store.UpdateLastUsedAtForOwner(context.Background(), record.Stage, record.CacheKey, "oidc:test:alice", next); err != nil {
		t.Fatalf("UpdateLastUsedAtForOwner returned error: %v", err)
	}
	gotAlice, ok, err = store.GetReadyForOwner(context.Background(), record.Stage, record.CacheKey, "oidc:test:alice")
	if err != nil || !ok {
		t.Fatalf("GetReadyForOwner alice after touch = ok %v err %v", ok, err)
	}
	gotBob, ok, err = store.GetReadyForOwner(context.Background(), record.Stage, record.CacheKey, "oidc:test:bob")
	if err != nil || !ok {
		t.Fatalf("GetReadyForOwner bob after touch = ok %v err %v", ok, err)
	}
	if !gotAlice.LastUsedAt.Equal(next) {
		t.Fatalf("unexpected alice last_used_at: got %s want %s", gotAlice.LastUsedAt.Format(time.RFC3339Nano), next.Format(time.RFC3339Nano))
	}
	if gotBob.LastUsedAt.Equal(next) {
		t.Fatalf("bob last_used_at changed when touching alice: %s", gotBob.LastUsedAt.Format(time.RFC3339Nano))
	}
}

func TestNewMigratesLegacyOwnerlessPrimaryKey(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "caches.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy cache db: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `
		CREATE TABLE cache_entries (
			cache_key TEXT NOT NULL,
			stage TEXT NOT NULL,
			reuse_mode TEXT,
			state TEXT NOT NULL,
			backend TEXT NOT NULL,
			architecture TEXT,
			runtime_base_key TEXT,
			policy_hash TEXT NOT NULL,
			policy_proto BLOB NOT NULL,
			repository_proto BLOB,
			repository_has_changeset INTEGER NOT NULL DEFAULT 0,
			repository_changeset_id TEXT,
			parent_cache_key TEXT,
			storage_driver TEXT NOT NULL DEFAULT 'file',
			storage_ref TEXT NOT NULL,
			storage_size_bytes INTEGER NOT NULL DEFAULT 0,
			exclusive_size_bytes INTEGER NOT NULL DEFAULT 0,
			driver_metadata TEXT,
			input_manifest_digest TEXT,
			command_digest TEXT,
			env_digest TEXT,
			normalized_outputs_digest TEXT,
			output_manifest_digest TEXT,
			dependency_key_files_digest TEXT,
			output_records_json BLOB,
			checkout_refresh_required INTEGER NOT NULL DEFAULT 0,
			imported_from_peer INTEGER NOT NULL DEFAULT 0,
			created_at_unix_nano INTEGER NOT NULL,
			last_used_at_unix_nano INTEGER NOT NULL,
			last_validated_at_unix_nano INTEGER NOT NULL DEFAULT 0,
			producer_version TEXT NOT NULL,
			PRIMARY KEY (stage, cache_key)
		)
	`); err != nil {
		t.Fatalf("create legacy cache table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy cache db: %v", err)
	}

	store, err := New(Options{MetadataDBPath: dbPath})
	if err != nil {
		t.Fatalf("New returned error for legacy cache db: %v", err)
	}
	record := Record{
		CacheKey:          "workspace-stage:test",
		Stage:             "workspace",
		State:             "ready",
		BackingSnapshotID: "snapshot-workspace-test",
		Backend:           "firecracker",
		PolicyHash:        "policy-hash",
		Policy:            testPolicy(),
		StorageRef:        "/tmp/workspace-test.ext4",
		StorageDriver:     "file",
		ProducerVersion:   "cleanroom-test/1",
	}
	alice := record
	alice.OwnerPrincipalID = "oidc:test:alice"
	alice.StorageRef = "/tmp/alice.ext4"
	bob := record
	bob.OwnerPrincipalID = "oidc:test:bob"
	bob.StorageRef = "/tmp/bob.ext4"
	if err := store.Create(context.Background(), alice); err != nil {
		t.Fatalf("Create alice after legacy migration returned error: %v", err)
	}
	if err := store.Create(context.Background(), bob); err != nil {
		t.Fatalf("Create bob with same stage/cache_key after legacy migration returned error: %v", err)
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
		CacheKey:          "runtime:test",
		Stage:             "runtime",
		State:             "ready",
		BackingSnapshotID: "snapshot-runtime-test",
		Backend:           "firecracker",
		PolicyHash:        "policy-hash",
		Policy:            testPolicy(),
		StorageRef:        "/tmp/runtime-test.ext4",
		StorageDriver:     "file",
		CreatedAt:         time.Unix(1700000000, 123).UTC(),
		LastUsedAt:        time.Unix(1700000000, 456).UTC(),
		ProducerVersion:   "cleanroom-test/1",
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
