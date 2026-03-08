package snapshotstore

import (
	"context"
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
		StorageRef: "/tmp/snap-test.ext4",
		CreatedAt:  time.Unix(1700000000, 0).UTC(),
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
	if got.SnapshotID != record.SnapshotID || got.PolicyHash != record.PolicyHash || got.StorageRef != record.StorageRef {
		t.Fatalf("unexpected snapshot record: %#v", got)
	}
	if got.Policy == nil || got.Policy.GetImageRef() != record.Policy.GetImageRef() {
		t.Fatalf("unexpected stored policy: %#v", got.Policy)
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
