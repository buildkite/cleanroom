package submodule

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initBareMirror(t *testing.T, sourceDir string) string {
	t.Helper()
	mirrorDir := t.TempDir()
	cmd := exec.Command("git", "clone", "--mirror", sourceDir, mirrorDir)
	cmd.Env = append(os.Environ(), "GIT_ALLOW_PROTOCOL=file")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git clone --mirror failed: %v\n%s", err, string(out))
	}
	return mirrorDir
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

func TestListMirrorSubmodulesAtCommit(t *testing.T) {
	subDir := initSubmoduleRepo(t)
	subMirrorDir := initBareMirror(t, subDir)
	subURL := "https://example.com/sub.git"

	superDir := t.TempDir()
	runGit(t, superDir, "init")
	runGit(t, superDir, "config", "user.name", "Cleanroom Test")
	runGit(t, superDir, "config", "user.email", "cleanroom-test@example.com")
	runGitWithEnv(t, superDir, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "add", subDir, "vendor/sub")
	runGit(t, superDir, "config", "-f", ".gitmodules", "submodule.vendor/sub.url", subURL)
	runGit(t, superDir, "add", ".gitmodules")
	runGit(t, superDir, "commit", "-m", "add submodule")

	superMirrorDir := initBareMirror(t, superDir)
	commitSHA := headCommit(t, superDir)

	var ensuredURL, ensuredSHA string
	ensureMirror := func(ctx context.Context, remoteURL, sha string) (string, error) {
		ensuredURL = remoteURL
		ensuredSHA = sha
		return subMirrorDir, nil
	}

	subs, err := ListMirrorSubmodulesAtCommit(context.Background(), superMirrorDir, commitSHA, ensureMirror)
	if err != nil {
		t.Fatalf("ListMirrorSubmodulesAtCommit returned error: %v", err)
	}
	if got, want := len(subs), 1; got != want {
		t.Fatalf("expected %d submodules, got %d", want, got)
	}
	if got, want := subs[0].Path, "vendor/sub"; got != want {
		t.Errorf("Path: got %q want %q", got, want)
	}
	if got, want := subs[0].RemoteURL, subURL; got != want {
		t.Errorf("RemoteURL: got %q want %q", got, want)
	}
	if subs[0].GitlinkSHA == "" {
		t.Error("expected non-empty GitlinkSHA")
	}
	if got, want := subs[0].MirrorDir, subMirrorDir; got != want {
		t.Errorf("MirrorDir: got %q want %q", got, want)
	}
	if got, want := ensuredURL, subURL; got != want {
		t.Errorf("ensureMirror called with URL %q, want %q", got, want)
	}
	if ensuredSHA == "" {
		t.Error("ensureMirror called with empty SHA")
	}
	_ = ensuredSHA
}

func TestListMirrorSubmodulesAtCommitNoGitmodules(t *testing.T) {
	plainDir := t.TempDir()
	runGit(t, plainDir, "init")
	runGit(t, plainDir, "config", "user.name", "Cleanroom Test")
	runGit(t, plainDir, "config", "user.email", "cleanroom-test@example.com")
	if err := os.WriteFile(filepath.Join(plainDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runGit(t, plainDir, "add", "README.md")
	runGit(t, plainDir, "commit", "-m", "initial")

	mirrorDir := initBareMirror(t, plainDir)
	commitSHA := headCommit(t, plainDir)

	ensureMirror := func(ctx context.Context, remoteURL, sha string) (string, error) {
		t.Error("ensureMirror should not be called when there are no submodules")
		return "", nil
	}

	subs, err := ListMirrorSubmodulesAtCommit(context.Background(), mirrorDir, commitSHA, ensureMirror)
	if err != nil {
		t.Fatalf("ListMirrorSubmodulesAtCommit returned error: %v", err)
	}
	if len(subs) != 0 {
		t.Errorf("expected empty submodule list, got %v", subs)
	}
}

func TestListMirrorSubmoduleFilesAtSHA(t *testing.T) {
	subDir := initSubmoduleRepo(t)
	if err := os.WriteFile(filepath.Join(subDir, "data.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write data.json: %v", err)
	}
	runGit(t, subDir, "add", "data.json")
	runGit(t, subDir, "commit", "-m", "add data.json")

	mirrorDir := initBareMirror(t, subDir)
	sha := headCommit(t, subDir)

	sm := MirrorSubmodule{
		Path:       "vendor/sub",
		RemoteURL:  "https://example.com/sub.git",
		GitlinkSHA: sha,
		MirrorDir:  mirrorDir,
	}

	files, err := ListMirrorSubmoduleFilesAtSHA(context.Background(), sm)
	if err != nil {
		t.Fatalf("ListMirrorSubmoduleFilesAtSHA returned error: %v", err)
	}

	wantPrefix := "vendor/sub/"
	for _, f := range files {
		if !strings.HasPrefix(f, wantPrefix) {
			t.Errorf("file %q missing expected prefix %q", f, wantPrefix)
		}
	}

	found := false
	for _, f := range files {
		if f == "vendor/sub/data.json" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected vendor/sub/data.json in files, got %v", files)
	}
}
