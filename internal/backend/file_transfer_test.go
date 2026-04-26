package backend

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSandboxFileUploadCommandAppliesMtime(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "artifact.txt")
	mtime := time.Unix(1700000123, 0)
	cmdArgs, err := SandboxFileUploadCommand(path, 0o640, mtime)
	if err != nil {
		t.Fatalf("SandboxFileUploadCommand returned error: %v", err)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = strings.NewReader("payload")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("upload command failed: %v\n%s", err, string(output))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if got, want := string(data), "payload"; got != want {
		t.Fatalf("unexpected uploaded payload: got %q want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat uploaded file: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o640); got != want {
		t.Fatalf("unexpected uploaded mode: got %04o want %04o", got, want)
	}
	if got, want := info.ModTime().Unix(), mtime.Unix(); got != want {
		t.Fatalf("unexpected uploaded mtime: got %d want %d", got, want)
	}
}

func TestSandboxTreeWalkScriptUsesBusyBoxCompatibleFindTerminator(t *testing.T) {
	t.Parallel()

	if !strings.Contains(SandboxTreeWalkScript, "cleanroom-walk {} \\;") {
		t.Fatalf("expected tree walk script to use BusyBox-compatible find terminator")
	}
	if strings.Contains(SandboxTreeWalkScript, "cleanroom-walk {} +") {
		t.Fatalf("expected tree walk script not to use GNU find batched -exec terminator")
	}
}

func TestSandboxFileReadCommandReportsMissingPathWithSentinel(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing.txt")
	cmdArgs, err := SandboxFileReadCommand(path, 32)
	if err != nil {
		t.Fatalf("SandboxFileReadCommand returned error: %v", err)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err == nil {
		t.Fatal("expected read command to reject missing path")
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected no stdout, got %q", stdout.String())
	}
	if got, want := strings.TrimSpace(stderr.String()), "path not found: "+path; got != want {
		t.Fatalf("unexpected stderr: got %q want %q", got, want)
	}
}

func TestSandboxFileUploadCommandRejectsDirectoryTarget(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "artifact-dir")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("create target directory: %v", err)
	}
	cmdArgs, err := SandboxFileUploadCommand(path, 0o640, time.Time{})
	if err != nil {
		t.Fatalf("SandboxFileUploadCommand returned error: %v", err)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = strings.NewReader("payload")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected upload command to reject directory target")
	}
	if !strings.Contains(string(output), "destination is a directory") {
		t.Fatalf("unexpected command output: %s", string(output))
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("read target directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected target directory to stay empty, found %d entries", len(entries))
	}
}

func TestSandboxFileUploadCommandRejectsSymlinkToDirectoryTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	targetDir := filepath.Join(dir, "target-dir")
	if err := os.Mkdir(targetDir, 0o755); err != nil {
		t.Fatalf("create target directory: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("target-dir", link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	cmdArgs, err := SandboxFileUploadCommand(link, 0o640, time.Time{})
	if err != nil {
		t.Fatalf("SandboxFileUploadCommand returned error: %v", err)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = strings.NewReader("payload")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected upload command to reject symlink-to-directory target")
	}
	if !strings.Contains(string(output), "destination is a directory") {
		t.Fatalf("unexpected command output: %s", string(output))
	}
	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected destination to remain a symlink, got mode %s", linkInfo.Mode())
	}
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		t.Fatalf("read target directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected target directory to stay empty, found %d entries", len(entries))
	}
}

func TestSandboxFileUploadCommandPreservesSymlinkTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink("target.txt", link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	cmdArgs, err := SandboxFileUploadCommand(link, 0o640, time.Time{})
	if err != nil {
		t.Fatalf("SandboxFileUploadCommand returned error: %v", err)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = strings.NewReader("payload")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("upload command failed: %v\n%s", err, string(output))
	}

	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected destination to remain a symlink, got mode %s", linkInfo.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if got, want := string(data), "payload"; got != want {
		t.Fatalf("unexpected target payload: got %q want %q", got, want)
	}
}

func TestSandboxFileUploadCommandPreservesSymlinkChainTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link2 := filepath.Join(dir, "link2.txt")
	if err := os.Symlink("target.txt", link2); err != nil {
		t.Fatalf("create second symlink: %v", err)
	}
	link1 := filepath.Join(dir, "link1.txt")
	if err := os.Symlink("link2.txt", link1); err != nil {
		t.Fatalf("create first symlink: %v", err)
	}
	cmdArgs, err := SandboxFileUploadCommand(link1, 0o640, time.Time{})
	if err != nil {
		t.Fatalf("SandboxFileUploadCommand returned error: %v", err)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = strings.NewReader("payload")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("upload command failed: %v\n%s", err, string(output))
	}

	for _, link := range []string{link1, link2} {
		linkInfo, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("lstat %s: %v", filepath.Base(link), err)
		}
		if linkInfo.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expected %s to remain a symlink, got mode %s", filepath.Base(link), linkInfo.Mode())
		}
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if got, want := string(data), "payload"; got != want {
		t.Fatalf("unexpected target payload: got %q want %q", got, want)
	}
}

func TestSandboxFileUploadCommandRejectsSymlinkLoop(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	link1 := filepath.Join(dir, "link1.txt")
	link2 := filepath.Join(dir, "link2.txt")
	if err := os.Symlink("link2.txt", link1); err != nil {
		t.Fatalf("create first symlink: %v", err)
	}
	if err := os.Symlink("link1.txt", link2); err != nil {
		t.Fatalf("create second symlink: %v", err)
	}
	cmdArgs, err := SandboxFileUploadCommand(link1, 0o640, time.Time{})
	if err != nil {
		t.Fatalf("SandboxFileUploadCommand returned error: %v", err)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = strings.NewReader("payload")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected upload command to reject symlink loop")
	}
	if !strings.Contains(string(output), "too many symlinks resolving destination") {
		t.Fatalf("unexpected command output: %s", string(output))
	}
	for _, link := range []string{link1, link2} {
		linkInfo, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("lstat %s: %v", filepath.Base(link), err)
		}
		if linkInfo.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expected %s to remain a symlink, got mode %s", filepath.Base(link), linkInfo.Mode())
		}
	}
}

func TestCopyReaderToAttachStdinClosesOnReadError(t *testing.T) {
	t.Parallel()

	readErr := errors.New("source failed")
	reader := &errorAfterDataReader{
		data: []byte("payload"),
		err:  readErr,
	}
	var got strings.Builder
	closed := false

	written, err := CopyReaderToAttachStdin(reader, AttachIO{
		WriteStdin: func(data []byte) error {
			_, err := got.Write(data)
			return err
		},
		CloseStdin: func() error {
			closed = true
			return nil
		},
	}, "file")

	if !errors.Is(err, readErr) {
		t.Fatalf("expected read error, got %v", err)
	}
	if got.String() != "payload" {
		t.Fatalf("unexpected stdin payload: got %q", got.String())
	}
	if written != int64(len("payload")) {
		t.Fatalf("unexpected written byte count: got %d want %d", written, len("payload"))
	}
	if !closed {
		t.Fatal("expected stdin to be closed after read error")
	}
}

func TestSandboxPathInfoDecoderDoesNotCorruptCompleteFieldsWithTrailingPartial(t *testing.T) {
	t.Parallel()

	var decoder SandboxPathInfoDecoder
	decoder.partial = append(make([]byte, 0, 128), []byte("/tmp/original")...)

	records, err := decoder.Write([]byte("\000file\00012\0000644\0001700000123\000\000/next-partial"))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected one complete record, got %d", len(records))
	}
	if got, want := records[0].Path, "/tmp/original"; got != want {
		t.Fatalf("unexpected decoded path: got %q want %q", got, want)
	}
	if got, want := decoder.partial, []byte("/next-partial"); string(got) != string(want) {
		t.Fatalf("unexpected trailing partial: got %q want %q", string(got), string(want))
	}
}

type errorAfterDataReader struct {
	data []byte
	err  error
	done bool
}

func (r *errorAfterDataReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.data), r.err
}
