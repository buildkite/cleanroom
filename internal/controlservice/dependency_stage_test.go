package controlservice

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/repositorystore"
)

const (
	testParentRemoteURL    = "https://github.com/buildkite/cleanroom.git"
	testSubmoduleRemoteURL = "https://github.com/buildkite/emojis.git"
)

func TestExpandStageKeyFilesAtCommitDoublestarGlob(t *testing.T) {
	repoDir := initControlServiceGitRepo(t)
	if err := os.MkdirAll(filepath.Join(repoDir, "vendor", "lib"), 0o755); err != nil {
		t.Fatalf("create vendor dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "vendor", "lib", "a.lock"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("write a.lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "vendor", "b.lock"), []byte("b\n"), 0o644); err != nil {
		t.Fatalf("write b.lock: %v", err)
	}
	runControlServiceGit(t, repoDir, "add", ".")
	runControlServiceGit(t, repoDir, "commit", "-m", "add vendor files")
	commitSHA := headControlServiceCommit(t, repoDir)

	expanded, err := expandStageKeyFilesAtCommit(context.Background(), repoDir, commitSHA, []string{"vendor/**"}, "dependency", nil)
	if err != nil {
		t.Fatalf("expandStageKeyFilesAtCommit returned error: %v", err)
	}
	if got, want := len(expanded), 2; got != want {
		t.Fatalf("unexpected expanded count: got %d want %d", got, want)
	}
	joined := strings.Join(expanded, ",")
	if !strings.Contains(joined, "vendor/lib/a.lock") {
		t.Fatalf("expected vendor/lib/a.lock in results, got %v", expanded)
	}
	if !strings.Contains(joined, "vendor/b.lock") {
		t.Fatalf("expected vendor/b.lock in results, got %v", expanded)
	}
}

func TestExpandStageKeyFilesAtCommitDoublestarGlobNoMatch(t *testing.T) {
	repoDir := initControlServiceGitRepo(t)
	commitSHA := headControlServiceCommit(t, repoDir)

	_, err := expandStageKeyFilesAtCommit(context.Background(), repoDir, commitSHA, []string{"vendor/**"}, "dependency", nil)
	if err == nil {
		t.Fatal("expected no-match error")
	}
	if !strings.Contains(err.Error(), "matched no files") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func initControlServiceGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runControlServiceGit(t, dir, "init")
	runControlServiceGit(t, dir, "config", "user.name", "Cleanroom Test")
	runControlServiceGit(t, dir, "config", "user.email", "cleanroom-test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runControlServiceGit(t, dir, "add", "README.md")
	runControlServiceGit(t, dir, "commit", "-m", "initial")
	return dir
}

func headControlServiceCommit(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD failed: %v\n%s", err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func runControlServiceGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	runControlServiceGitWithEnv(t, dir, nil, args...)
}

func runControlServiceGitWithEnv(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}

func initControlServiceGitRepoWithSubmodule(t *testing.T) (superDir, subDir, superMirror, subMirror string) {
	t.Helper()

	subDir = t.TempDir()
	runControlServiceGit(t, subDir, "init")
	runControlServiceGit(t, subDir, "config", "user.name", "Cleanroom Test")
	runControlServiceGit(t, subDir, "config", "user.email", "cleanroom-test@example.com")
	if err := os.WriteFile(filepath.Join(subDir, "README.md"), []byte("submodule\n"), 0o644); err != nil {
		t.Fatalf("write submodule README: %v", err)
	}
	runControlServiceGit(t, subDir, "add", "README.md")
	runControlServiceGit(t, subDir, "commit", "-m", "initial submodule commit")

	superDir = t.TempDir()
	runControlServiceGit(t, superDir, "init")
	runControlServiceGit(t, superDir, "config", "user.name", "Cleanroom Test")
	runControlServiceGit(t, superDir, "config", "user.email", "cleanroom-test@example.com")
	if err := os.WriteFile(filepath.Join(superDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write super README: %v", err)
	}
	runControlServiceGit(t, superDir, "add", "README.md")
	runControlServiceGit(t, superDir, "commit", "-m", "initial")
	runControlServiceGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "add", subDir, "vendor/emojis")
	runControlServiceGit(t, superDir, "config", "-f", ".gitmodules", "submodule.vendor/emojis.url", testSubmoduleRemoteURL)
	runControlServiceGit(t, superDir, "add", ".gitmodules")
	runControlServiceGit(t, superDir, "commit", "-m", "add submodule")

	subMirror = t.TempDir()
	runControlServiceGit(t, subMirror, "clone", "--mirror", subDir, subMirror)

	superMirror = t.TempDir()
	runControlServiceGit(t, superMirror, "clone", "--mirror", superDir, superMirror)

	return superDir, subDir, superMirror, subMirror
}

type testSubmoduleStore struct {
	mirrorDir string
	calls     []testSubmoduleMirrorCall
}

type testSubmoduleMirrorCall struct {
	URL string
	SHA string
}

func (s *testSubmoduleStore) EnsureCommit(_ context.Context, _, _ string, _ repositorystore.FetchHints) error {
	return nil
}

func (s *testSubmoduleStore) ReadFileAtCommit(_ context.Context, _, _, _ string) ([]byte, error) {
	return nil, nil
}

func (s *testSubmoduleStore) WithRepository(_ context.Context, _, _ string, _ repositorystore.FetchHints, fn func(string) error) error {
	return fn(s.mirrorDir)
}

func (s *testSubmoduleStore) TransportHints(_ context.Context, _, _ string, _ repositorystore.FetchHints) (repositorystore.TransportHints, error) {
	return repositorystore.TransportHints{}, nil
}

func (s *testSubmoduleStore) EnsureSubmoduleMirror(_ context.Context, submoduleRemoteURL, gitlinkSHA string) (string, error) {
	s.calls = append(s.calls, testSubmoduleMirrorCall{URL: submoduleRemoteURL, SHA: gitlinkSHA})
	return s.mirrorDir, nil
}

func TestStageInputFilesDigestAtCommitDigestsSubmoduleFiles(t *testing.T) {
	superDir, subDir, superMirror, subMirror := initControlServiceGitRepoWithSubmodule(t)

	if err := os.WriteFile(filepath.Join(subDir, "emoji1.json"), []byte(`{"name":"smile"}`), 0o644); err != nil {
		t.Fatalf("write emoji1.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "emoji2.json"), []byte(`{"name":"wink"}`), 0o644); err != nil {
		t.Fatalf("write emoji2.json: %v", err)
	}
	runControlServiceGit(t, subDir, "add", "emoji1.json", "emoji2.json")
	runControlServiceGit(t, subDir, "commit", "-m", "add emojis")
	runControlServiceGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "update", "--remote", "vendor/emojis")
	runControlServiceGit(t, superDir, "add", "vendor/emojis")
	runControlServiceGit(t, superDir, "commit", "-m", "update submodule to include emojis")
	commitSHA := headControlServiceCommit(t, superDir)

	runControlServiceGit(t, subMirror, "fetch", "--all")
	runControlServiceGit(t, superMirror, "fetch", "--all")

	store := &testSubmoduleStore{mirrorDir: subMirror}

	digest, err := stageInputFilesDigestAtCommit(context.Background(), superMirror, testParentRemoteURL, commitSHA, []string{"vendor/emojis/**"}, "test", true, store)
	if err != nil {
		t.Fatalf("stageInputFilesDigestAtCommit: %v", err)
	}
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("expected sha256 digest, got %q", digest)
	}
	if len(store.calls) == 0 {
		t.Fatal("expected EnsureSubmoduleMirror to be called")
	}
}

func TestStageInputFilesDigestAtCommitMatchesChangesetPath(t *testing.T) {
	superDir, subDir, superMirror, subMirror := initControlServiceGitRepoWithSubmodule(t)

	if err := os.WriteFile(filepath.Join(subDir, "emoji1.json"), []byte(`{"name":"smile"}`), 0o644); err != nil {
		t.Fatalf("write emoji1.json: %v", err)
	}
	runControlServiceGit(t, subDir, "add", "emoji1.json")
	runControlServiceGit(t, subDir, "commit", "-m", "add emoji")
	runControlServiceGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "update", "--remote", "vendor/emojis")
	runControlServiceGit(t, superDir, "add", "vendor/emojis")
	runControlServiceGit(t, superDir, "commit", "-m", "update submodule")
	commitSHA := headControlServiceCommit(t, superDir)

	runControlServiceGit(t, subMirror, "fetch", "--all")
	runControlServiceGit(t, superMirror, "fetch", "--all")

	if err := os.WriteFile(filepath.Join(superDir, "trigger.txt"), []byte("trigger changeset\n"), 0o644); err != nil {
		t.Fatalf("write trigger.txt: %v", err)
	}

	checkout := &repositorycheckout.Checkout{
		RemoteURL:      testParentRemoteURL,
		CommitSHA:      commitSHA,
		DestinationDir: "/workspace",
		Submodules:     true,
	}

	changeset, err := repositorychangeset.BuildFromWorkingTree(superDir, checkout)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected changeset")
	}

	pattern := []string{"vendor/emojis/**"}

	changesetFiles, err := changeset.DigestRegularFilesFromBaseWithOptions(superDir, pattern, repositorychangeset.DigestPathsOptions{Submodules: true})
	if err != nil {
		t.Fatalf("DigestRegularFilesFromBaseWithOptions: %v", err)
	}
	changesetManifest := make([]stageInputFileDigest, 0, len(changesetFiles))
	for _, f := range changesetFiles {
		changesetManifest = append(changesetManifest, stageInputFileDigest{
			Path:   f.Path,
			Mode:   f.Mode,
			SHA256: f.SHA256,
		})
	}
	changesetDigest, err := digestStageInputFileManifest(changesetManifest, "test")
	if err != nil {
		t.Fatalf("digestStageInputFileManifest: %v", err)
	}

	store := &testSubmoduleStore{mirrorDir: subMirror}
	atCommitDigest, err := stageInputFilesDigestAtCommit(context.Background(), superMirror, testParentRemoteURL, commitSHA, pattern, "test", true, store)
	if err != nil {
		t.Fatalf("stageInputFilesDigestAtCommit: %v", err)
	}

	if changesetDigest != atCommitDigest {
		t.Fatalf("digest mismatch: changeset=%q at-commit=%q", changesetDigest, atCommitDigest)
	}
}

// TestStageInputFilesDigestWithChangesetResolvesSubmoduleViaMirror exercises
// the production path: digestPathsFromBase running against a bare mirror of
// the parent (no working tree, no initialised submodules), with submodule
// contents resolved from the submodule's own bare mirror at the gitlink SHA.
// This is the case that produced "fatal: bad object :vendor/emojis" before.
func TestStageInputFilesDigestWithChangesetResolvesSubmoduleViaMirror(t *testing.T) {
	superDir, subDir, superMirror, subMirror := initControlServiceGitRepoWithSubmodule(t)

	if err := os.WriteFile(filepath.Join(subDir, "emoji1.json"), []byte(`{"name":"smile"}`), 0o644); err != nil {
		t.Fatalf("write emoji1.json: %v", err)
	}
	runControlServiceGit(t, subDir, "add", "emoji1.json")
	runControlServiceGit(t, subDir, "commit", "-m", "add emoji")
	runControlServiceGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "update", "--remote", "vendor/emojis")
	runControlServiceGit(t, superDir, "add", "vendor/emojis")
	runControlServiceGit(t, superDir, "commit", "-m", "update submodule")
	commitSHA := headControlServiceCommit(t, superDir)

	runControlServiceGit(t, subMirror, "fetch", "--all")
	runControlServiceGit(t, superMirror, "fetch", "--all")

	if err := os.WriteFile(filepath.Join(superDir, "trigger.txt"), []byte("trigger changeset\n"), 0o644); err != nil {
		t.Fatalf("write trigger.txt: %v", err)
	}

	checkout := &repositorycheckout.Checkout{
		RemoteURL:      testParentRemoteURL,
		CommitSHA:      commitSHA,
		DestinationDir: "/workspace",
		Submodules:     true,
	}
	changeset, err := repositorychangeset.BuildFromWorkingTree(superDir, checkout)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected changeset")
	}

	store := &testSubmoduleStore{mirrorDir: subMirror}

	digest, err := stageInputFilesDigestWithChangeset(context.Background(), superMirror, changeset, []string{"vendor/emojis/**"}, "test", testParentRemoteURL, true, store)
	if err != nil {
		t.Fatalf("stageInputFilesDigestWithChangeset (submodules on, mirror): %v", err)
	}
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("expected sha256 digest, got %q", digest)
	}
	if len(store.calls) == 0 {
		t.Fatal("expected EnsureSubmoduleMirror to be called")
	}
}

// TestStageKeyFilesDigestWithChangesetResolvesGitlinkLiteralViaMirror is the
// exact regression for the original bug: a literal `vendor/emojis` (the
// gitlink itself) hitting the key-files path against a bare mirror. Before
// the fix this produced "fatal: bad object :vendor/emojis"; after, it must
// either succeed or surface a meaningful gitlink error.
func TestStageKeyFilesDigestWithChangesetResolvesGitlinkLiteralViaMirror(t *testing.T) {
	superDir, subDir, superMirror, subMirror := initControlServiceGitRepoWithSubmodule(t)

	if err := os.WriteFile(filepath.Join(subDir, "emoji1.json"), []byte(`{"name":"smile"}`), 0o644); err != nil {
		t.Fatalf("write emoji1.json: %v", err)
	}
	runControlServiceGit(t, subDir, "add", "emoji1.json")
	runControlServiceGit(t, subDir, "commit", "-m", "add emoji")
	runControlServiceGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "update", "--remote", "vendor/emojis")
	runControlServiceGit(t, superDir, "add", "vendor/emojis")
	runControlServiceGit(t, superDir, "commit", "-m", "update submodule")
	commitSHA := headControlServiceCommit(t, superDir)

	runControlServiceGit(t, subMirror, "fetch", "--all")
	runControlServiceGit(t, superMirror, "fetch", "--all")

	if err := os.WriteFile(filepath.Join(superDir, "trigger.txt"), []byte("trigger changeset\n"), 0o644); err != nil {
		t.Fatalf("write trigger.txt: %v", err)
	}

	checkout := &repositorycheckout.Checkout{
		RemoteURL:      testParentRemoteURL,
		CommitSHA:      commitSHA,
		DestinationDir: "/workspace",
		Submodules:     true,
	}
	changeset, err := repositorychangeset.BuildFromWorkingTree(superDir, checkout)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected changeset")
	}

	store := &testSubmoduleStore{mirrorDir: subMirror}

	digest, err := stageKeyFilesDigestWithChangeset(context.Background(), superMirror, changeset, []string{"vendor/emojis/**"}, "dependency", testParentRemoteURL, true, store)
	if err != nil {
		t.Fatalf("stageKeyFilesDigestWithChangeset (mirror, glob): %v", err)
	}
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("expected sha256 digest, got %q", digest)
	}
}

func TestStageInputFilesDigestAtCommitNoGitmodulesAtCommit(t *testing.T) {
	repoDir := initControlServiceGitRepo(t)
	if err := os.MkdirAll(filepath.Join(repoDir, "src"), 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "src", "file.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatalf("write src/file.txt: %v", err)
	}
	runControlServiceGit(t, repoDir, "add", ".")
	runControlServiceGit(t, repoDir, "commit", "-m", "add src")
	commitSHA := headControlServiceCommit(t, repoDir)

	mirror := t.TempDir()
	runControlServiceGit(t, mirror, "clone", "--mirror", repoDir, mirror)

	store := &testSubmoduleStore{mirrorDir: mirror}

	digest, err := stageInputFilesDigestAtCommit(context.Background(), mirror, "", commitSHA, []string{"src/**"}, "test", true, store)
	if err != nil {
		t.Fatalf("stageInputFilesDigestAtCommit: %v", err)
	}
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("expected sha256 digest, got %q", digest)
	}
}

func TestStageInputFilesDigestAtCommitRejectsGitlinkLiteral(t *testing.T) {
	superDir, _, superMirror, subMirror := initControlServiceGitRepoWithSubmodule(t)
	commitSHA := headControlServiceCommit(t, superDir)

	runControlServiceGit(t, superMirror, "fetch", "--all")

	store := &testSubmoduleStore{mirrorDir: subMirror}

	_, err := stageInputFilesDigestAtCommit(context.Background(), superMirror, testParentRemoteURL, commitSHA, []string{"vendor/emojis"}, "test", true, store)
	if err == nil {
		t.Fatal("expected error for gitlink literal")
	}
	if !strings.Contains(err.Error(), "gitlink") {
		t.Fatalf("expected gitlink error, got %v", err)
	}
}

func TestStageKeyFilesDigestAtCommitRejectsGitlinkLiteral(t *testing.T) {
	superDir, _, superMirror, subMirror := initControlServiceGitRepoWithSubmodule(t)
	commitSHA := headControlServiceCommit(t, superDir)

	runControlServiceGit(t, superMirror, "fetch", "--all")

	store := &testSubmoduleStore{mirrorDir: subMirror}

	_, err := stageKeyFilesDigestAtCommit(context.Background(), superMirror, testParentRemoteURL, commitSHA, []string{"vendor/emojis"}, "dependency", true, store)
	if err == nil {
		t.Fatal("expected error for gitlink literal")
	}
	if !strings.Contains(err.Error(), "is a gitlink") {
		t.Fatalf("expected gitlink error, got %v", err)
	}
}

func TestStageKeyFilesDigestAtCommitOptInErrorForSubmoduleGlob(t *testing.T) {
	superDir, _, superMirror, _ := initControlServiceGitRepoWithSubmodule(t)
	commitSHA := headControlServiceCommit(t, superDir)

	runControlServiceGit(t, superMirror, "fetch", "--all")

	_, err := stageKeyFilesDigestAtCommit(context.Background(), superMirror, "", commitSHA, []string{"vendor/emojis/**"}, "dependency", false, nil)
	if err == nil {
		t.Fatal("expected error for submodule glob with submodules disabled")
	}
	if !strings.Contains(err.Error(), "is inside submodule") && !strings.Contains(err.Error(), "matched no files") && !strings.Contains(err.Error(), "is a gitlink") {
		t.Fatalf("unexpected error: %v", err)
	}
}
