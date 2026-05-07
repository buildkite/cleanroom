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
	if err := os.Chmod(filepath.Join(repoDir, "new.txt"), 0o755); err != nil {
		t.Fatalf("chmod new file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "A.txt"), []byte("decoy\n"), 0o755); err != nil {
		t.Fatalf("write decoy file: %v", err)
	}
	if err := os.Chmod(filepath.Join(repoDir, "A.txt"), 0o755); err != nil {
		t.Fatalf("chmod decoy file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "[AB].txt"), []byte("literal brackets\n"), 0o644); err != nil {
		t.Fatalf("write bracket file: %v", err)
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
	modes := make(map[string]string, len(changeset.Files))
	for _, file := range changeset.Files {
		modes[file.Path] = file.Mode
	}
	if got, want := modes["README.md"], "100644"; got != want {
		t.Fatalf("unexpected README.md mode: got %q want %q", got, want)
	}
	if got, want := modes["new.txt"], "100755"; got != want {
		t.Fatalf("unexpected new.txt mode: got %q want %q", got, want)
	}
	if got, want := modes["A.txt"], "100755"; got != want {
		t.Fatalf("unexpected A.txt mode: got %q want %q", got, want)
	}
	if got, want := modes["[AB].txt"], "100644"; got != want {
		t.Fatalf("unexpected [AB].txt mode: got %q want %q", got, want)
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
	runGit(t, filepath.Join(superDir, "deps/sub"), "config", "user.name", "Cleanroom Test")
	runGit(t, filepath.Join(superDir, "deps/sub"), "config", "user.email", "cleanroom-test@example.com")
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

func TestValidateContentAllowsContextLineResemblingGitlinkIndex(t *testing.T) {
	patch := []byte(strings.Join([]string{
		"diff --git a/README.md b/README.md",
		"index 1111111111111111111111111111111111111111..2222222222222222222222222222222222222222 100644",
		"--- a/README.md",
		"+++ b/README.md",
		"@@ -1,3 +1,3 @@",
		" index 3333333333333333333333333333333333333333 160000",
		"-before",
		"+after",
		" unchanged",
		"",
	}, "\n"))
	changeset := &Changeset{
		Format:        FormatGitDiffV1,
		BaseCommitSHA: "0123456789abcdef0123456789abcdef01234567",
		TreeDigest:    "89abcdef0123456789abcdef0123456789abcdef",
		Patch:         patch,
		Files: []File{{
			Path:   "README.md",
			SHA256: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		}},
	}
	changeset.Digest = buildDigest(changeset.BaseCommitSHA, changeset.TreeDigest, changeset.Patch, changeset.Files)

	if err := changeset.ValidateContent(); err != nil {
		t.Fatalf("expected ValidateContent to allow ordinary patch context line, got %v", err)
	}
}

func TestValidateContentAllowsOrdinarySubprojectCommitText(t *testing.T) {
	patch := []byte(strings.Join([]string{
		"diff --git a/README.md b/README.md",
		"index 1111111111111111111111111111111111111111..2222222222222222222222222222222222222222 100644",
		"--- a/README.md",
		"+++ b/README.md",
		"@@ -1 +1,2 @@",
		" existing",
		"+Subproject commit 2222222222222222222222222222222222222222",
		"",
	}, "\n"))
	changeset := &Changeset{
		Format:        FormatGitDiffV1,
		BaseCommitSHA: "0123456789abcdef0123456789abcdef01234567",
		TreeDigest:    "89abcdef0123456789abcdef0123456789abcdef",
		Patch:         patch,
		Files: []File{{
			Path:   "README.md",
			SHA256: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		}},
	}
	changeset.Digest = buildDigest(changeset.BaseCommitSHA, changeset.TreeDigest, changeset.Patch, changeset.Files)

	if err := changeset.ValidateContent(); err != nil {
		t.Fatalf("expected ValidateContent to allow ordinary added text, got %v", err)
	}
}

func TestBuildFromWorkingTreePreservesSignificantWhitespaceInPaths(t *testing.T) {
	repoDir := initGitRepository(t)
	pathWithSpaces := " file.txt "
	if err := os.WriteFile(filepath.Join(repoDir, pathWithSpaces), []byte("spaced path\n"), 0o644); err != nil {
		t.Fatalf("write spaced file: %v", err)
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
	if digest, deleted, ok := changeset.ChangedFileDigest(pathWithSpaces); !ok || deleted || digest == "" {
		t.Fatalf("expected spaced path digest, got digest=%q deleted=%v ok=%v", digest, deleted, ok)
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
	if got, want := digests[0].Mode, "100644"; got != want {
		t.Fatalf("unexpected README.md mode: got %q want %q", got, want)
	}
}

func TestDigestRegularFilesFromBaseRejectsSymlink(t *testing.T) {
	repoDir := initGitRepository(t)
	if err := os.WriteFile(filepath.Join(repoDir, "real.lock"), []byte("real\n"), 0o644); err != nil {
		t.Fatalf("write real.lock: %v", err)
	}
	if err := os.Symlink("real.lock", filepath.Join(repoDir, "link.lock")); err != nil {
		t.Fatalf("symlink link.lock: %v", err)
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

	_, err = changeset.DigestRegularFilesFromBase(repoDir, []string{"link.lock"})
	if err == nil {
		t.Fatal("expected symlink input to fail")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink error, got %v", err)
	}
}

func TestResetCommandsUseDoubleForceClean(t *testing.T) {
	checkout := &repositorycheckout.Checkout{
		CommitSHA:      "0123456789abcdef0123456789abcdef01234567",
		DestinationDir: "/workspace",
	}
	changeset := &Changeset{
		BaseCommitSHA: checkout.CommitSHA,
		TreeDigest:    "89abcdef0123456789abcdef0123456789abcdef",
		Patch:         []byte("diff --git a/README.md b/README.md\n"),
	}

	resetCommand := strings.Join(ResetCommand(checkout), "\n")
	if !strings.Contains(resetCommand, `git -C "$dest" clean -ffd >/dev/null`) {
		t.Fatalf("expected reset command to double-force clean, got %q", resetCommand)
	}

	applyCommand := strings.Join(ApplyCommandResettingCheckout(checkout, changeset), "\n")
	if !strings.Contains(applyCommand, `git -C "$dest" clean -ffd >/dev/null`) {
		t.Fatalf("expected apply-reset command to double-force clean, got %q", applyCommand)
	}
}

func TestDigestPathsFromBaseExpandsDoublestarGlobs(t *testing.T) {
	repoDir := initGitRepository(t)
	if err := os.MkdirAll(filepath.Join(repoDir, "subdir", "a"), 0o755); err != nil {
		t.Fatalf("create nested dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "subdir", "a", "b.txt"), []byte("nested\n"), 0o644); err != nil {
		t.Fatalf("write nested file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "subdir", "top.txt"), []byte("top\n"), 0o644); err != nil {
		t.Fatalf("write top file: %v", err)
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

	digests, err := changeset.DigestPathsFromBase(repoDir, []string{"subdir/**"})
	if err != nil {
		t.Fatalf("DigestPathsFromBase returned error: %v", err)
	}
	if got, want := len(digests), 2; got != want {
		t.Fatalf("unexpected digest count: got %d want %d", got, want)
	}
	paths := make([]string, len(digests))
	for i, d := range digests {
		paths[i] = d.Path
	}
	if !strings.Contains(strings.Join(paths, ","), "subdir/a/b.txt") {
		t.Fatalf("expected subdir/a/b.txt in results, got %v", paths)
	}

	_, err = changeset.DigestPathsFromBase(repoDir, []string{"subdir/**/missing.txt"})
	if err == nil {
		t.Fatal("expected empty doublestar glob to fail")
	}
	if !strings.Contains(err.Error(), "matched no files") {
		t.Fatalf("unexpected empty glob error: %v", err)
	}
}

func TestDigestRegularFilesFromBaseExpandsDoublestarGlobs(t *testing.T) {
	repoDir := initGitRepository(t)
	if err := os.MkdirAll(filepath.Join(repoDir, "vendor", "pkg"), 0o755); err != nil {
		t.Fatalf("create vendor dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "vendor", "pkg", "lib.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatalf("write lib.go: %v", err)
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

	digests, err := changeset.DigestRegularFilesFromBase(repoDir, []string{"vendor/**"})
	if err != nil {
		t.Fatalf("DigestRegularFilesFromBase returned error: %v", err)
	}
	if got, want := len(digests), 1; got != want {
		t.Fatalf("unexpected digest count: got %d want %d", got, want)
	}
	if got, want := digests[0].Path, "vendor/pkg/lib.go"; got != want {
		t.Fatalf("unexpected digest path: got %q want %q", got, want)
	}

	_, err = changeset.DigestRegularFilesFromBase(repoDir, []string{"vendor/**/missing.go"})
	if err == nil {
		t.Fatal("expected empty doublestar glob to fail")
	}
	if !strings.Contains(err.Error(), "matched no files") {
		t.Fatalf("unexpected empty glob error: %v", err)
	}
}

func TestDigestPathsFromBaseExpandsGlobs(t *testing.T) {
	repoDir := initGitRepository(t)
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module example.com/test\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "go.sum"), []byte("example.com/test v0.0.0 h1:abc123\n"), 0o644); err != nil {
		t.Fatalf("write go.sum: %v", err)
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

	digests, err := changeset.DigestPathsFromBase(repoDir, []string{"*.sum", "go.*"})
	if err != nil {
		t.Fatalf("DigestPathsFromBase returned error: %v", err)
	}
	if got, want := len(digests), 2; got != want {
		t.Fatalf("unexpected digest count: got %d want %d", got, want)
	}
	if got, want := digests[0].Path, "go.mod"; got != want {
		t.Fatalf("unexpected first digest path: got %q want %q", got, want)
	}
	if got, want := digests[1].Path, "go.sum"; got != want {
		t.Fatalf("unexpected second digest path: got %q want %q", got, want)
	}

	_, err = changeset.DigestPathsFromBase(repoDir, []string{"*.missing"})
	if err == nil {
		t.Fatal("expected empty glob to fail")
	}
	if !strings.Contains(err.Error(), "matched no files") {
		t.Fatalf("unexpected empty glob error: %v", err)
	}
}

func TestDigestPathsFromBaseExpandsDeletedGlobMatches(t *testing.T) {
	repoDir := initGitRepository(t)
	if err := os.WriteFile(filepath.Join(repoDir, "go.sum"), []byte("example.com/test v0.0.0 h1:abc123\n"), 0o644); err != nil {
		t.Fatalf("write go.sum: %v", err)
	}
	runGit(t, repoDir, "add", "go.sum")
	runGit(t, repoDir, "commit", "-m", "add go sum")

	checkout := &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      headCommit(t, repoDir),
		DestinationDir: "/workspace",
	}

	if err := os.Remove(filepath.Join(repoDir, "go.sum")); err != nil {
		t.Fatalf("delete go.sum: %v", err)
	}
	changeset, err := BuildFromWorkingTree(repoDir, checkout)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree returned error: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected changeset")
	}

	digests, err := changeset.DigestPathsFromBase(repoDir, []string{"*.sum"})
	if err != nil {
		t.Fatalf("DigestPathsFromBase returned error: %v", err)
	}
	if got, want := len(digests), 1; got != want {
		t.Fatalf("unexpected digest count: got %d want %d", got, want)
	}
	if got, want := digests[0].Path, "go.sum"; got != want {
		t.Fatalf("unexpected digest path: got %q want %q", got, want)
	}
	if !digests[0].Deleted {
		t.Fatal("expected go.sum to be recorded as deleted")
	}
	if digests[0].SHA256 != "" {
		t.Fatalf("expected deleted go.sum digest to be empty, got %q", digests[0].SHA256)
	}
}

func TestWorktreeNameStatusCommandReportsWorkingTreeChanges(t *testing.T) {
	repoDir := initGitRepository(t)
	if err := os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatalf("write gitignore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "removed.txt"), []byte("remove me\n"), 0o644); err != nil {
		t.Fatalf("write removed file: %v", err)
	}
	runGit(t, repoDir, "add", ".gitignore", "removed.txt")
	runGit(t, repoDir, "commit", "-m", "add fixtures")
	base := headCommit(t, repoDir)

	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatalf("rewrite readme: %v", err)
	}
	if err := os.Remove(filepath.Join(repoDir, "removed.txt")); err != nil {
		t.Fatalf("remove tracked file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatalf("write new file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "ignored"), 0o755); err != nil {
		t.Fatalf("create ignored dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "ignored", "output.txt"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatalf("write ignored file: %v", err)
	}

	command := WorktreeNameStatusCommand(&repositorycheckout.Checkout{
		CommitSHA:      base,
		DestinationDir: repoDir,
	})
	out := runShellCommand(t, command)
	for _, want := range []string{
		"M\x00README.md\x00",
		"D\x00removed.txt\x00",
		"A\x00new.txt\x00",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected name-status output to contain %q, got %q", want, out)
		}
	}
	if strings.Contains(out, "ignored") {
		t.Fatalf("expected ignored files to stay out of name-status output, got %q", out)
	}
}

func TestWorktreeChangesCommandsRejectDirtySubmoduleWorktree(t *testing.T) {
	submoduleDir := initGitRepository(t)
	superDir := t.TempDir()
	runGit(t, superDir, "init")
	runGit(t, superDir, "config", "user.name", "Cleanroom Test")
	runGit(t, superDir, "config", "user.email", "cleanroom-test@example.com")
	runGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "add", submoduleDir, "deps/sub")
	runGit(t, superDir, "commit", "-m", "add submodule")
	base := headCommit(t, superDir)

	if err := os.WriteFile(filepath.Join(superDir, "deps/sub/README.md"), []byte("dirty submodule\n"), 0o644); err != nil {
		t.Fatalf("rewrite submodule readme: %v", err)
	}

	checkout := &repositorycheckout.Checkout{
		CommitSHA:      base,
		DestinationDir: superDir,
		Submodules:     true,
	}
	for _, tc := range []struct {
		name    string
		command []string
	}{
		{name: "name-status", command: WorktreeNameStatusCommand(checkout)},
		{name: "diff", command: WorktreeDiffCommand(checkout)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runShellCommandResult(t, tc.command)
			if err == nil {
				t.Fatalf("expected dirty submodule worktree error, got output %q", out)
			}
			if !strings.Contains(out, "dirty submodule worktree") {
				t.Fatalf("expected dirty submodule worktree error, got %v\n%s", err, out)
			}
		})
	}
}

func TestWorktreeChangesCommandsRejectSubmoduleGitlinkChange(t *testing.T) {
	submoduleDir := initGitRepository(t)
	superDir := t.TempDir()
	runGit(t, superDir, "init")
	runGit(t, superDir, "config", "user.name", "Cleanroom Test")
	runGit(t, superDir, "config", "user.email", "cleanroom-test@example.com")
	runGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "add", submoduleDir, "deps/sub")
	runGit(t, superDir, "commit", "-m", "add submodule")
	base := headCommit(t, superDir)

	if err := os.WriteFile(filepath.Join(superDir, "deps/sub/README.md"), []byte("advanced submodule\n"), 0o644); err != nil {
		t.Fatalf("rewrite submodule readme: %v", err)
	}
	runGit(t, filepath.Join(superDir, "deps/sub"), "add", "README.md")
	runGit(t, filepath.Join(superDir, "deps/sub"), "config", "user.name", "Cleanroom Test")
	runGit(t, filepath.Join(superDir, "deps/sub"), "config", "user.email", "cleanroom-test@example.com")
	runGit(t, filepath.Join(superDir, "deps/sub"), "commit", "-m", "advance submodule")

	checkout := &repositorycheckout.Checkout{
		CommitSHA:      base,
		DestinationDir: superDir,
		Submodules:     true,
	}
	for _, tc := range []struct {
		name    string
		command []string
	}{
		{name: "name-status", command: WorktreeNameStatusCommand(checkout)},
		{name: "diff", command: WorktreeDiffCommand(checkout)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runShellCommandResult(t, tc.command)
			if err == nil {
				t.Fatalf("expected submodule gitlink error, got output %q", out)
			}
			if !strings.Contains(out, "submodule gitlink change") {
				t.Fatalf("expected submodule gitlink error, got %v\n%s", err, out)
			}
		})
	}
}

func initGitRepositoryWithSubmodule(t *testing.T) (superDir, subDir string) {
	t.Helper()
	subDir = t.TempDir()
	runGit(t, subDir, "init")
	runGit(t, subDir, "config", "user.name", "Cleanroom Test")
	runGit(t, subDir, "config", "user.email", "cleanroom-test@example.com")
	if err := os.WriteFile(filepath.Join(subDir, "README.md"), []byte("submodule\n"), 0o644); err != nil {
		t.Fatalf("write submodule README: %v", err)
	}
	runGit(t, subDir, "add", "README.md")
	runGit(t, subDir, "commit", "-m", "initial submodule commit")

	superDir = t.TempDir()
	runGit(t, superDir, "init")
	runGit(t, superDir, "config", "user.name", "Cleanroom Test")
	runGit(t, superDir, "config", "user.email", "cleanroom-test@example.com")
	if err := os.WriteFile(filepath.Join(superDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write super README: %v", err)
	}
	runGit(t, superDir, "add", "README.md")
	runGit(t, superDir, "commit", "-m", "initial")
	runGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "add", subDir, "vendor/emojis")
	runGit(t, superDir, "commit", "-m", "add submodule")
	return superDir, subDir
}

func TestDigestRegularFilesFromBaseExpandsSubmoduleGlob(t *testing.T) {
	superDir, subDir := initGitRepositoryWithSubmodule(t)

	if err := os.WriteFile(filepath.Join(subDir, "emoji1.json"), []byte(`{"name":"smile"}`), 0o644); err != nil {
		t.Fatalf("write emoji1.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "emoji2.json"), []byte(`{"name":"wink"}`), 0o644); err != nil {
		t.Fatalf("write emoji2.json: %v", err)
	}
	runGit(t, subDir, "add", "emoji1.json", "emoji2.json")
	runGit(t, subDir, "commit", "-m", "add emojis")
	runGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "update", "--remote", "vendor/emojis")
	runGit(t, superDir, "add", "vendor/emojis")
	runGit(t, superDir, "commit", "-m", "update submodule to include emojis")

	if err := os.WriteFile(filepath.Join(superDir, "change.txt"), []byte("trigger changeset\n"), 0o644); err != nil {
		t.Fatalf("write change.txt: %v", err)
	}

	checkout := &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      headCommit(t, superDir),
		DestinationDir: "/workspace",
	}

	changeset, err := BuildFromWorkingTree(superDir, checkout)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected changeset")
	}

	files, err := changeset.DigestRegularFilesFromBaseWithOptions(superDir, []string{"vendor/emojis/**"}, DigestPathsOptions{Submodules: true})
	if err != nil {
		t.Fatalf("DigestRegularFilesFromBaseWithOptions: %v", err)
	}
	wantPaths := []string{
		"vendor/emojis/README.md",
		"vendor/emojis/emoji1.json",
		"vendor/emojis/emoji2.json",
	}
	if got, want := len(files), len(wantPaths); got != want {
		gotPaths := make([]string, len(files))
		for i, f := range files {
			gotPaths[i] = f.Path
		}
		t.Fatalf("unexpected file count: got %d want %d, paths: %v", got, want, gotPaths)
	}
	for i, f := range files {
		if f.Path != wantPaths[i] {
			t.Fatalf("unexpected path at index %d: got %q want %q", i, f.Path, wantPaths[i])
		}
		if f.Mode != "100644" {
			t.Fatalf("expected mode 100644 for %q, got %q", f.Path, f.Mode)
		}
		if !strings.HasPrefix(f.SHA256, "sha256:") {
			t.Fatalf("expected sha256 digest for %q, got %q", f.Path, f.SHA256)
		}
	}
}

func TestDigestRegularFilesFromBaseRejectsGitlinkLiteral(t *testing.T) {
	superDir, _ := initGitRepositoryWithSubmodule(t)

	if err := os.WriteFile(filepath.Join(superDir, "change.txt"), []byte("trigger changeset\n"), 0o644); err != nil {
		t.Fatalf("write change.txt: %v", err)
	}

	checkout := &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      headCommit(t, superDir),
		DestinationDir: "/workspace",
	}

	changeset, err := BuildFromWorkingTree(superDir, checkout)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected changeset")
	}

	_, err = changeset.DigestRegularFilesFromBaseWithOptions(superDir, []string{"vendor/emojis"}, DigestPathsOptions{Submodules: true})
	if err == nil {
		t.Fatal("expected error for literal gitlink path")
	}
	if !strings.Contains(err.Error(), "gitlink") {
		t.Fatalf("expected gitlink error, got %v", err)
	}
}

func TestDigestRegularFilesFromBaseRequiresSubmodulesOptIn(t *testing.T) {
	superDir, subDir := initGitRepositoryWithSubmodule(t)

	if err := os.WriteFile(filepath.Join(subDir, "emoji1.json"), []byte(`{"name":"smile"}`), 0o644); err != nil {
		t.Fatalf("write emoji1.json: %v", err)
	}
	runGit(t, subDir, "add", "emoji1.json")
	runGit(t, subDir, "commit", "-m", "add emoji")
	runGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "update", "--remote", "vendor/emojis")
	runGit(t, superDir, "add", "vendor/emojis")
	runGit(t, superDir, "commit", "-m", "update submodule")

	if err := os.WriteFile(filepath.Join(superDir, "change.txt"), []byte("trigger\n"), 0o644); err != nil {
		t.Fatalf("write change.txt: %v", err)
	}

	checkout := &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      headCommit(t, superDir),
		DestinationDir: "/workspace",
	}

	changeset, err := BuildFromWorkingTree(superDir, checkout)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected changeset")
	}

	_, err = changeset.DigestRegularFilesFromBaseWithOptions(superDir, []string{"vendor/emojis/**"}, DigestPathsOptions{Submodules: false})
	if err == nil {
		t.Fatal("expected error when Submodules is false and pattern resolves into submodule")
	}
	if !strings.Contains(err.Error(), "submodule") && !strings.Contains(err.Error(), "gitlink") {
		t.Fatalf("expected submodule or gitlink error, got %v", err)
	}
}

func TestDigestRegularFilesFromBaseRejectsUninitialisedSubmodule(t *testing.T) {
	superDir, _ := initGitRepositoryWithSubmodule(t)

	if err := os.RemoveAll(filepath.Join(superDir, "vendor/emojis/.git")); err != nil {
		t.Fatalf("remove submodule .git: %v", err)
	}

	if err := os.WriteFile(filepath.Join(superDir, "change.txt"), []byte("trigger\n"), 0o644); err != nil {
		t.Fatalf("write change.txt: %v", err)
	}

	checkout := &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      headCommit(t, superDir),
		DestinationDir: "/workspace",
	}

	changeset, err := BuildFromWorkingTree(superDir, checkout)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected changeset")
	}

	_, err = changeset.DigestRegularFilesFromBaseWithOptions(superDir, []string{"vendor/emojis/**"}, DigestPathsOptions{Submodules: true})
	if err == nil {
		t.Fatal("expected error for uninitialised submodule")
	}
	if !strings.Contains(err.Error(), "initialised") && !strings.Contains(err.Error(), "initialized") && !strings.Contains(err.Error(), "submodule") {
		t.Fatalf("expected uninitialised submodule error, got %v", err)
	}
}

func TestDigestRegularFilesFromBaseStableAcrossRuns(t *testing.T) {
	superDir, subDir := initGitRepositoryWithSubmodule(t)

	if err := os.WriteFile(filepath.Join(subDir, "a.json"), []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatalf("write a.json: %v", err)
	}
	runGit(t, subDir, "add", "a.json")
	runGit(t, subDir, "commit", "-m", "add a")
	runGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "update", "--remote", "vendor/emojis")
	runGit(t, superDir, "add", "vendor/emojis")
	runGit(t, superDir, "commit", "-m", "update submodule")

	if err := os.WriteFile(filepath.Join(superDir, "change.txt"), []byte("trigger\n"), 0o644); err != nil {
		t.Fatalf("write change.txt: %v", err)
	}

	checkout := &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      headCommit(t, superDir),
		DestinationDir: "/workspace",
	}

	changeset, err := BuildFromWorkingTree(superDir, checkout)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected changeset")
	}

	opts := DigestPathsOptions{Submodules: true}

	first, err := changeset.DigestRegularFilesFromBaseWithOptions(superDir, []string{"vendor/emojis/**"}, opts)
	if err != nil {
		t.Fatalf("first DigestRegularFilesFromBaseWithOptions: %v", err)
	}

	second, err := changeset.DigestRegularFilesFromBaseWithOptions(superDir, []string{"vendor/emojis/**"}, opts)
	if err != nil {
		t.Fatalf("second DigestRegularFilesFromBaseWithOptions: %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("file count mismatch: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Path != second[i].Path || first[i].SHA256 != second[i].SHA256 {
			t.Fatalf("result differs at index %d: %+v vs %+v", i, first[i], second[i])
		}
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

func runShellCommand(t *testing.T, command []string) string {
	t.Helper()
	out, err := runShellCommandResult(t, command)
	if err != nil {
		t.Fatalf("command %v failed: %v\n%s", command, err, out)
	}
	return out
}

func runShellCommandResult(t *testing.T, command []string) (string, error) {
	t.Helper()
	if len(command) == 0 {
		t.Fatal("missing command")
	}
	cmd := exec.Command(command[0], command[1:]...)
	out, err := cmd.CombinedOutput()
	return string(out), err
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
