package changesetstore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

func TestStorePutGetListDelete(t *testing.T) {
	t.Parallel()

	store, err := New(Options{
		MetadataDBPath: filepath.Join(t.TempDir(), "changesets.db"),
		PayloadDir:     filepath.Join(t.TempDir(), "payloads"),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	repository, changeset := testRepositoryChangeset(t)

	record, err := store.Put(context.Background(), repository, changeset)
	if err != nil {
		t.Fatalf("Put returned error: %v", err)
	}
	if record.ChangesetID == "" {
		t.Fatal("expected changeset id")
	}
	if record.CanonicalRemoteURL != repository.RemoteURL {
		t.Fatalf("unexpected remote URL: got %q want %q", record.CanonicalRemoteURL, repository.RemoteURL)
	}
	if record.BaseCommitSHA != changeset.BaseCommitSHA {
		t.Fatalf("unexpected base commit: got %q want %q", record.BaseCommitSHA, changeset.BaseCommitSHA)
	}
	if record.SubmoduleMode != "enabled" {
		t.Fatalf("unexpected submodule mode: got %q want enabled", record.SubmoduleMode)
	}
	if record.TransportFormat != TransportFormatProtoV1 {
		t.Fatalf("unexpected transport format: got %q want %q", record.TransportFormat, TransportFormatProtoV1)
	}
	if record.TransportRef == "" {
		t.Fatal("expected transport ref")
	}
	if record.PayloadDigest == "" {
		t.Fatal("expected payload digest")
	}

	gotRecord, gotChangeset, ok, err := store.Get(context.Background(), record.ChangesetID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected stored changeset")
	}
	if gotRecord.ChangesetID != record.ChangesetID || gotRecord.ChangesetDigest != record.ChangesetDigest || gotRecord.FinalTreeDigest != record.FinalTreeDigest {
		t.Fatalf("unexpected record: %#v", gotRecord)
	}
	if gotChangeset.Digest != changeset.Digest || string(gotChangeset.Patch) != string(changeset.Patch) {
		t.Fatalf("unexpected changeset payload: %#v", gotChangeset)
	}

	items, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got, want := len(items), 1; got != want {
		t.Fatalf("unexpected changeset count: got %d want %d", got, want)
	}

	if err := store.Delete(context.Background(), record.ChangesetID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if _, _, ok, err := store.Get(context.Background(), record.ChangesetID); err != nil {
		t.Fatalf("Get after delete returned error: %v", err)
	} else if ok {
		t.Fatal("expected changeset to be deleted")
	}
}

func TestStorePutDeduplicatesByChangesetIdentity(t *testing.T) {
	t.Parallel()

	store, err := New(Options{
		MetadataDBPath: filepath.Join(t.TempDir(), "changesets.db"),
		PayloadDir:     filepath.Join(t.TempDir(), "payloads"),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	repository, changeset := testRepositoryChangeset(t)
	first, err := store.Put(context.Background(), repository, changeset)
	if err != nil {
		t.Fatalf("first Put returned error: %v", err)
	}
	time.Sleep(time.Millisecond)
	second, err := store.Put(context.Background(), repository, changeset)
	if err != nil {
		t.Fatalf("second Put returned error: %v", err)
	}
	if second.ChangesetID != first.ChangesetID {
		t.Fatalf("expected same changeset id, got %q then %q", first.ChangesetID, second.ChangesetID)
	}

	items, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got, want := len(items), 1; got != want {
		t.Fatalf("expected one deduplicated changeset, got %d", got)
	}
}

func TestStorePutRejectsBaseCommitMismatch(t *testing.T) {
	t.Parallel()

	store, err := New(Options{
		MetadataDBPath: filepath.Join(t.TempDir(), "changesets.db"),
		PayloadDir:     filepath.Join(t.TempDir(), "payloads"),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	repository, changeset := testRepositoryChangeset(t)
	repository.CommitSHA = "ffffffffffffffffffffffffffffffffffffffff"
	if _, err := store.Put(context.Background(), repository, changeset); err == nil {
		t.Fatal("expected base commit mismatch error")
	}
}

func TestStoreGetDetectsPayloadCorruption(t *testing.T) {
	t.Parallel()

	store, err := New(Options{
		MetadataDBPath: filepath.Join(t.TempDir(), "changesets.db"),
		PayloadDir:     filepath.Join(t.TempDir(), "payloads"),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	repository, changeset := testRepositoryChangeset(t)
	record, err := store.Put(context.Background(), repository, changeset)
	if err != nil {
		t.Fatalf("Put returned error: %v", err)
	}
	payloadPath, err := store.payloadPath(record)
	if err != nil {
		t.Fatalf("payloadPath returned error: %v", err)
	}
	if err := os.WriteFile(payloadPath, []byte("corrupt"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if _, _, _, err := store.Get(context.Background(), record.ChangesetID); err == nil {
		t.Fatal("expected payload corruption error")
	}
}

func TestStoreDeleteDoesNotRequireReadablePayload(t *testing.T) {
	t.Parallel()

	store, err := New(Options{
		MetadataDBPath: filepath.Join(t.TempDir(), "changesets.db"),
		PayloadDir:     filepath.Join(t.TempDir(), "payloads"),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	repository, changeset := testRepositoryChangeset(t)
	record, err := store.Put(context.Background(), repository, changeset)
	if err != nil {
		t.Fatalf("Put returned error: %v", err)
	}
	payloadPath, err := store.payloadPath(record)
	if err != nil {
		t.Fatalf("payloadPath returned error: %v", err)
	}
	if err := os.WriteFile(payloadPath, []byte("corrupt"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	if err := store.Delete(context.Background(), record.ChangesetID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if _, _, ok, err := store.Get(context.Background(), record.ChangesetID); err != nil {
		t.Fatalf("Get after delete returned error: %v", err)
	} else if ok {
		t.Fatal("expected changeset metadata to be deleted")
	}
}

func TestStoreRejectsInvalidTransportRef(t *testing.T) {
	t.Parallel()

	store, err := New(Options{
		MetadataDBPath: filepath.Join(t.TempDir(), "changesets.db"),
		PayloadDir:     filepath.Join(t.TempDir(), "payloads"),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	invalidRefs := []string{"", "../escape.pb", "/tmp/escape.pb"}
	for _, transportRef := range invalidRefs {
		record := Record{
			ChangesetID:  "changeset:v1:test",
			TransportRef: transportRef,
		}
		if _, err := store.payloadPath(record); err == nil {
			t.Fatalf("expected transport ref %q to be rejected", transportRef)
		}
	}
}

func testRepositoryChangeset(t *testing.T) (*repositorycheckout.Checkout, *repositorychangeset.Changeset) {
	t.Helper()

	repoDir := t.TempDir()
	runTestGit(t, repoDir, "init", ".")
	runTestGit(t, repoDir, "config", "user.email", "cleanroom@example.com")
	runTestGit(t, repoDir, "config", "user.name", "Cleanroom Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(README.md) returned error: %v", err)
	}
	runTestGit(t, repoDir, "add", ".")
	runTestGit(t, repoDir, "commit", "-m", "initial")
	commitSHA := strings.TrimSpace(runTestGit(t, repoDir, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("hello from changeset\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(README.md) returned error: %v", err)
	}

	repository := &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      commitSHA,
		DestinationDir: "/workspace",
		Submodules:     true,
	}
	changeset, err := repositorychangeset.BuildFromWorkingTree(repoDir, repository)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree returned error: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected repository changeset")
	}
	return repository, changeset
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s returned error: %v\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output)
}
