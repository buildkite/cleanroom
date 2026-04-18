package repositorychangeset

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

func TestBuildFromWorkingTree(t *testing.T) {
	repoDir := initGitRepository(t)
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("hello from changeset\n"), 0o644); err != nil {
		t.Fatalf("rewrite readme: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "new.txt"), []byte("new file\n"), 0o755); err != nil {
		t.Fatalf("write new file: %v", err)
	}

	checkout := &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      headCommit(t, repoDir),
		DestinationDir: "/workspace",
	}

	changeset, err := BuildFromWorkingTree(repoDir, checkout)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree returned error: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected changeset")
	}
	if err := changeset.ValidateForCheckout(checkout); err != nil {
		t.Fatalf("ValidateForCheckout returned error: %v", err)
	}
	if changeset.Format != FormatGitDiffV1 {
		t.Fatalf("unexpected format: got %q want %q", changeset.Format, FormatGitDiffV1)
	}
	if !strings.HasPrefix(changeset.Digest, "sha256:") {
		t.Fatalf("expected digest to use sha256 prefix, got %q", changeset.Digest)
	}
	if changeset.TreeDigest == "" {
		t.Fatal("expected tree digest")
	}
	if len(changeset.Patch) == 0 {
		t.Fatal("expected patch bytes")
	}
	if !bytesContainAll(changeset.Patch, []byte("README.md"), []byte("new.txt")) {
		t.Fatalf("expected patch to mention changed files, got %q", string(changeset.Patch))
	}
	if digest, deleted, ok := changeset.ChangedFileDigest("README.md"); !ok || deleted || digest == "" {
		t.Fatalf("expected README.md digest, got digest=%q deleted=%v ok=%v", digest, deleted, ok)
	}
	if digest, deleted, ok := changeset.ChangedFileDigest("new.txt"); !ok || deleted || digest == "" {
		t.Fatalf("expected new.txt digest, got digest=%q deleted=%v ok=%v", digest, deleted, ok)
	}

	again, err := BuildFromWorkingTree(repoDir, checkout)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree second run returned error: %v", err)
	}
	if again == nil {
		t.Fatal("expected second changeset")
	}
	if changeset.Digest != again.Digest {
		t.Fatalf("expected stable digest, got %q then %q", changeset.Digest, again.Digest)
	}
	if changeset.TreeDigest != again.TreeDigest {
		t.Fatalf("expected stable tree digest, got %q then %q", changeset.TreeDigest, again.TreeDigest)
	}
}

func TestBuildFromWorkingTreeReturnsNilForCleanRepo(t *testing.T) {
	repoDir := initGitRepository(t)
	checkout := &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      headCommit(t, repoDir),
		DestinationDir: "/workspace",
	}

	changeset, err := BuildFromWorkingTree(repoDir, checkout)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree returned error: %v", err)
	}
	if changeset != nil {
		t.Fatalf("expected nil changeset for clean repo, got %+v", changeset)
	}
}

func TestBuildFromWorkingTreeRejectsDirtySubmoduleWorktree(t *testing.T) {
	submoduleDir := initGitRepository(t)
	superDir := t.TempDir()
	runGit(t, superDir, "init")
	runGit(t, superDir, "config", "user.name", "Cleanroom Test")
	runGit(t, superDir, "config", "user.email", "cleanroom-test@example.com")
	runGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "add", submoduleDir, "deps/sub")
	runGit(t, superDir, "commit", "-m", "add submodule")

	if err := os.WriteFile(filepath.Join(superDir, "deps/sub/README.md"), []byte("dirty submodule\n"), 0o644); err != nil {
		t.Fatalf("rewrite submodule readme: %v", err)
	}

	checkout := &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      headCommit(t, superDir),
		DestinationDir: "/workspace",
		Submodules:     true,
	}

	changeset, err := BuildFromWorkingTree(superDir, checkout)
	if err == nil {
		t.Fatalf("expected dirty submodule worktree error, got changeset %+v", changeset)
	}
	if !strings.Contains(err.Error(), "dirty submodule worktree") {
		t.Fatalf("expected dirty submodule worktree error, got %v", err)
	}
}

func TestBuildFromWorkingTreeRejectsSubmoduleGitlinkChange(t *testing.T) {
	submoduleDir := initGitRepository(t)
	superDir := t.TempDir()
	runGit(t, superDir, "init")
	runGit(t, superDir, "config", "user.name", "Cleanroom Test")
	runGit(t, superDir, "config", "user.email", "cleanroom-test@example.com")
	runGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "add", submoduleDir, "deps/sub")
	runGit(t, superDir, "commit", "-m", "add submodule")

	if err := os.WriteFile(filepath.Join(superDir, "deps/sub/README.md"), []byte("advanced submodule\n"), 0o644); err != nil {
		t.Fatalf("rewrite submodule readme: %v", err)
	}
	runGit(t, filepath.Join(superDir, "deps/sub"), "add", "README.md")
	runGit(t, filepath.Join(superDir, "deps/sub"), "commit", "-m", "advance submodule")
	runGit(t, superDir, "add", "deps/sub")

	checkout := &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      headCommit(t, superDir),
		DestinationDir: "/workspace",
		Submodules:     true,
	}

	changeset, err := BuildFromWorkingTree(superDir, checkout)
	if err == nil {
		t.Fatalf("expected submodule gitlink error, got changeset %+v", changeset)
	}
	if !strings.Contains(err.Error(), "submodule gitlink change") {
		t.Fatalf("expected submodule gitlink error, got %v", err)
	}
}

func TestValidateContentRejectsSubmoduleGitlinkPatch(t *testing.T) {
	patch := []byte(strings.Join([]string{
		"diff --git a/deps/sub b/deps/sub",
		"index 1111111111111111111111111111111111111111..2222222222222222222222222222222222222222 160000",
		"--- a/deps/sub",
		"+++ b/deps/sub",
		"@@ -1 +1 @@",
		"-Subproject commit 1111111111111111111111111111111111111111",
		"+Subproject commit 2222222222222222222222222222222222222222",
		"",
	}, "\n"))
	changeset := &Changeset{
		Format:        FormatGitDiffV1,
		BaseCommitSHA: "0123456789abcdef0123456789abcdef01234567",
		TreeDigest:    "89abcdef0123456789abcdef0123456789abcdef",
		Patch:         patch,
		Files: []File{{
			Path:   "deps/sub",
			SHA256: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		}},
	}
	changeset.Digest = buildDigest(changeset.BaseCommitSHA, changeset.TreeDigest, changeset.Patch, changeset.Files)

	if err := changeset.ValidateContent(); err == nil {
		t.Fatal("expected ValidateContent to reject submodule gitlink patch")
	} else if !strings.Contains(err.Error(), "submodule gitlink") {
		t.Fatalf("expected submodule gitlink error, got %v", err)
	}
}

func TestDigestPathsFromBaseUsesPatchedContents(t *testing.T) {
	repoDir := initGitRepository(t)
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("patched\n"), 0o644); err != nil {
		t.Fatalf("rewrite readme: %v", err)
	}

	checkout := &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      headCommit(t, repoDir),
		DestinationDir: "/workspace",
	}

	changeset, err := BuildFromWorkingTree(repoDir, checkout)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree returned error: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected changeset")
	}

	digests, err := changeset.DigestPathsFromBase(repoDir, []string{"README.md"})
	if err != nil {
		t.Fatalf("DigestPathsFromBase returned error: %v", err)
	}
	if got, want := len(digests), 1; got != want {
		t.Fatalf("unexpected digest count: got %d want %d", got, want)
	}
	if got, want := digests[0].SHA256, sha256Digest([]byte("patched\n")); got != want {
		t.Fatalf("unexpected README.md digest: got %q want %q", got, want)
	}
	if digests[0].Deleted {
		t.Fatal("expected README.md to be present after applying changeset")
	}
}

func initGitRepository(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Cleanroom Test")
	runGit(t, dir, "config", "user.email", "cleanroom-test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "initial")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/buildkite/cleanroom.git")
	return dir
}

func headCommit(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD failed: %v\n%s", err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func bytesContainAll(haystack []byte, needles ...[]byte) bool {
	for _, needle := range needles {
		if !strings.Contains(string(haystack), string(needle)) {
			return false
		}
	}
	return true
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return runGitWithEnv(t, dir, nil, args...)
}

func runGitWithEnv(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
	return string(out)
}
