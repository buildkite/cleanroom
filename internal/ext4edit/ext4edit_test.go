package ext4edit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/hosttools"
)

func TestDebugFSCommandQuotesArguments(t *testing.T) {
	t.Parallel()

	got, err := debugFSCommand("write", `/tmp/src with "quotes"`, `/dir with space/file\name`)
	if err != nil {
		t.Fatalf("debugFSCommand: %v", err)
	}

	want := `write "/tmp/src with \"quotes\"" "/dir with space/file\\name"`
	if got != want {
		t.Fatalf("unexpected command: got %q want %q", got, want)
	}
}

func TestDebugFSSetInodeFieldCommandQuotesPath(t *testing.T) {
	t.Parallel()

	got, err := debugFSSetInodeFieldCommand(`/dir with space/file "quoted"`, "mode", "0100755")
	if err != nil {
		t.Fatalf("debugFSSetInodeFieldCommand: %v", err)
	}

	want := `set_inode_field "/dir with space/file \"quoted\"" mode 0100755`
	if got != want {
		t.Fatalf("unexpected command: got %q want %q", got, want)
	}
}

func TestDebugFSStatLinkTargetParsesFastLinkDest(t *testing.T) {
	t.Parallel()

	output := "debugfs 1.47.3 (8-Jul-2025)\nFast link dest: \"/usr/bin\"\n"
	got, err := DebugFSStatLinkTarget(output)
	if err != nil {
		t.Fatalf("DebugFSStatLinkTarget: %v", err)
	}
	if got != "/usr/bin" {
		t.Fatalf("unexpected symlink target: got %q want %q", got, "/usr/bin")
	}
}

func TestDebugFSQuoteArgRejectsControlCharacters(t *testing.T) {
	t.Parallel()

	if _, err := debugFSQuoteArg("/tmp/path\nwith-newline"); err == nil {
		t.Fatal("expected control characters to be rejected")
	}
}

func TestDebugFSCommandOutputErrorReportsMissingFile(t *testing.T) {
	t.Parallel()

	output := "debugfs 1.47.3 (8-Jul-2025)\n/sbin: File not found by ext2_lookup while looking up \"/sbin\"\n"
	got := DebugFSCommandOutputError(output)
	if got == "" {
		t.Fatal("expected missing-file debugfs output to be treated as an error")
	}
}

func TestDebugFSCommandOutputErrorReportsUnknownCommand(t *testing.T) {
	t.Parallel()

	output := "debugfs 1.47.3 (8-Jul-2025)\ndebugfs: Command not found writee\n"
	got := DebugFSCommandOutputError(output)
	if got == "" {
		t.Fatal("expected unknown-command debugfs output to be treated as an error")
	}
}

func TestDebugFSCommandOutputErrorReportsUnreadableFilesystem(t *testing.T) {
	t.Parallel()

	output := "debugfs 1.47.3 (8-Jul-2025)\nInode bitmap checksum does not match bitmap while reading allocation bitmaps\nstat: Filesystem not open\n"
	got := DebugFSCommandOutputError(output)
	if got == "" {
		t.Fatal("expected unreadable-filesystem debugfs output to be treated as an error")
	}
}

func TestDebugFSCommandOutputErrorIgnoresSuccessfulOutput(t *testing.T) {
	t.Parallel()

	output := "debugfs 1.47.3 (8-Jul-2025)\nInode: 531   Type: regular    Mode:  0755   Flags: 0x80000\n"
	if got := DebugFSCommandOutputError(output); got != "" {
		t.Fatalf("expected empty error for successful debugfs output, got %q", got)
	}
}

func TestDebugFSStatTypeParsesDirectory(t *testing.T) {
	t.Parallel()

	output := "debugfs 1.47.3 (8-Jul-2025)\nInode: 261   Type: directory    Mode:  0755   Flags: 0x80000\n"
	if got, want := DebugFSStatType(output), PathKindDirectory; got != want {
		t.Fatalf("unexpected stat type: got %q want %q", got, want)
	}
}

func TestDebugFSStatTypeParsesSymlink(t *testing.T) {
	t.Parallel()

	output := "debugfs 1.47.3 (8-Jul-2025)\nInode: 82   Type: symlink    Mode:  0755   Flags: 0x0\n"
	if got, want := DebugFSStatType(output), PathKindSymlink; got != want {
		t.Fatalf("unexpected stat type: got %q want %q", got, want)
	}
}

func TestDebugFSStatModeParsesPermissions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		output string
		want   os.FileMode
	}{
		{
			name:   "permissions only",
			output: "debugfs 1.47.3 (8-Jul-2025)\nInode: 531   Type: regular    Mode:  0755   Flags: 0x80000\n",
			want:   0o755,
		},
		{
			name:   "mode with inode type bits",
			output: "debugfs 1.47.3 (8-Jul-2025)\nInode: 531   Type: regular    Mode:  0100644   Flags: 0x80000\n",
			want:   0o644,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := DebugFSStatMode(tc.output)
			if !ok {
				t.Fatal("expected stat mode to parse")
			}
			if got != tc.want {
				t.Fatalf("unexpected stat mode: got %#o want %#o", got, tc.want)
			}
		})
	}
}

func TestPathTypeWithErrorReturnsInspectionError(t *testing.T) {
	t.Parallel()

	debugfsBinary, _, ok := requireDebugFSTools(t)
	if !ok {
		return
	}
	if debugfsBinary == "" {
		t.Fatal("expected debugfs path")
	}

	imagePath := filepath.Join(t.TempDir(), "not-ext4.img")
	if err := os.WriteFile(imagePath, []byte("not an ext4 image"), 0o600); err != nil {
		t.Fatalf("write invalid image: %v", err)
	}

	_, err := PathTypeWithError(imagePath, "/")
	if err == nil {
		t.Fatal("expected invalid image inspection to fail")
	}
	if !strings.Contains(err.Error(), "debugfs") {
		t.Fatalf("expected debugfs error, got %v", err)
	}
}

func TestPathIsExecutableFollowsFinalSymlinkAndChecksMode(t *testing.T) {
	debugfsBinary, mkfsBinary, ok := requireDebugFSTools(t)
	if !ok {
		return
	}

	imagePath := createExt4Image(t, mkfsBinary)
	createExt4Dir(t, debugfsBinary, imagePath, "/usr")
	createExt4Dir(t, debugfsBinary, imagePath, "/usr/bin")
	createExt4Dir(t, debugfsBinary, imagePath, "/usr/local")
	createExt4Dir(t, debugfsBinary, imagePath, "/usr/local/bin")

	tmpDir := t.TempDir()
	executableSrc := filepath.Join(tmpDir, "dockerd-real")
	if err := os.WriteFile(executableSrc, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write executable source file: %v", err)
	}
	if err := InjectFile(imagePath, executableSrc, "/usr/local/bin/dockerd-real", 0o755); err != nil {
		t.Fatalf("InjectFile executable: %v", err)
	}
	createExt4Symlink(t, debugfsBinary, imagePath, "/usr/bin/dockerd", "/usr/local/bin/dockerd-real")

	nonExecutableSrc := filepath.Join(tmpDir, "not-dockerd")
	if err := os.WriteFile(nonExecutableSrc, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write non-executable source file: %v", err)
	}
	if err := InjectFile(imagePath, nonExecutableSrc, "/usr/local/bin/not-dockerd", 0o644); err != nil {
		t.Fatalf("InjectFile non-executable: %v", err)
	}
	createExt4Symlink(t, debugfsBinary, imagePath, "/usr/bin/not-dockerd", "/usr/local/bin/not-dockerd")
	createExt4Symlink(t, debugfsBinary, imagePath, "/usr/bin/broken-dockerd", "/missing/dockerd")

	for _, tc := range []struct {
		path string
		want bool
	}{
		{path: "/usr/bin/dockerd", want: true},
		{path: "/usr/local/bin/dockerd-real", want: true},
		{path: "/usr/bin/not-dockerd", want: false},
		{path: "/usr/local/bin/not-dockerd", want: false},
		{path: "/usr/bin/broken-dockerd", want: false},
		{path: "/usr/bin/missing-dockerd", want: false},
		{path: "/usr/bin", want: false},
	} {
		got, err := PathIsExecutable(imagePath, tc.path)
		if err != nil {
			t.Fatalf("PathIsExecutable(%q): %v", tc.path, err)
		}
		if got != tc.want {
			t.Fatalf("PathIsExecutable(%q): got %v want %v", tc.path, got, tc.want)
		}
	}
}

func TestInjectFileFollowsSymlinkedParentDirectories(t *testing.T) {
	debugfsBinary, mkfsBinary, ok := requireDebugFSTools(t)
	if !ok {
		return
	}

	imagePath := createExt4Image(t, mkfsBinary)
	createExt4Dir(t, debugfsBinary, imagePath, "/usr")
	createExt4Dir(t, debugfsBinary, imagePath, "/usr/bin")
	createExt4Symlink(t, debugfsBinary, imagePath, "/usr/sbin", "/usr/bin")

	srcPath := filepath.Join(t.TempDir(), "cleanroom init with spaces.sh")
	if err := os.WriteFile(srcPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	if err := InjectFile(imagePath, srcPath, "/usr/sbin/cleanroom-init", 0o755); err != nil {
		t.Fatalf("InjectFile: %v", err)
	}

	if got, want := PathType(imagePath, "/usr/sbin"), PathKindSymlink; got != want {
		t.Fatalf("unexpected /usr/sbin type: got %q want %q", got, want)
	}
	if !PathExists(imagePath, "/usr/sbin/cleanroom-init") {
		t.Fatal("expected injected file to be reachable via symlinked /usr/sbin path")
	}
	if !PathExists(imagePath, "/usr/bin/cleanroom-init") {
		t.Fatal("expected injected file to be written to the symlink target directory")
	}
}

func TestInjectFileMakesReadonlyImageOwnerWritable(t *testing.T) {
	debugfsBinary, mkfsBinary, ok := requireDebugFSTools(t)
	if !ok {
		return
	}

	imagePath := createExt4Image(t, mkfsBinary)
	createExt4Dir(t, debugfsBinary, imagePath, "/usr")
	createExt4Dir(t, debugfsBinary, imagePath, "/usr/local")
	createExt4Dir(t, debugfsBinary, imagePath, "/usr/local/bin")

	if err := os.Chmod(imagePath, 0o444); err != nil {
		t.Fatalf("chmod readonly image: %v", err)
	}

	srcPath := filepath.Join(t.TempDir(), "guest-agent")
	if err := os.WriteFile(srcPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	if err := InjectFile(imagePath, srcPath, "/usr/local/bin/cleanroom-guest-agent", 0o755); err != nil {
		t.Fatalf("InjectFile: %v", err)
	}

	info, err := os.Stat(imagePath)
	if err != nil {
		t.Fatalf("stat image: %v", err)
	}
	if info.Mode().Perm()&0o200 == 0 {
		t.Fatalf("expected image to become owner-writable, got mode %o", info.Mode().Perm())
	}
	if !PathExists(imagePath, "/usr/local/bin/cleanroom-guest-agent") {
		t.Fatal("expected injected file to exist after mutating a readonly image")
	}
}

func requireDebugFSTools(t *testing.T) (debugfsBinary, mkfsBinary string, ok bool) {
	t.Helper()

	debugfsBinary, err := hosttools.ResolveE2FSProgsBinary("debugfs")
	if err != nil {
		t.Skipf("debugfs unavailable: %v", err)
		return "", "", false
	}
	mkfsBinary, err = hosttools.ResolveE2FSProgsBinary("mkfs.ext4")
	if err != nil {
		t.Skipf("mkfs.ext4 unavailable: %v", err)
		return "", "", false
	}
	return debugfsBinary, mkfsBinary, true
}

func createExt4Image(t *testing.T, mkfsBinary string) string {
	t.Helper()

	imagePath := filepath.Join(t.TempDir(), "test.ext4")
	f, err := os.Create(imagePath)
	if err != nil {
		t.Fatalf("create ext4 image: %v", err)
	}
	if err := f.Truncate(16 * 1024 * 1024); err != nil {
		_ = f.Close()
		t.Fatalf("size ext4 image: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close ext4 image: %v", err)
	}
	if out, err := exec.Command(mkfsBinary, "-q", "-F", imagePath).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v (%s)", err, string(out))
	}
	return imagePath
}

func createExt4Dir(t *testing.T, debugfsBinary, imagePath, dir string) {
	t.Helper()
	runDebugFSCommand(t, debugfsBinary, imagePath, "mkdir", dir)
}

func createExt4Symlink(t *testing.T, debugfsBinary, imagePath, linkPath, target string) {
	t.Helper()

	command, err := debugFSCommand("symlink", linkPath, target)
	if err != nil {
		t.Fatalf("build symlink command: %v", err)
	}
	if out, err := exec.Command(debugfsBinary, "-w", "-R", command, imagePath).CombinedOutput(); err != nil {
		t.Fatalf("debugfs symlink: %v (%s)", err, string(out))
	}
}

func runDebugFSCommand(t *testing.T, debugfsBinary, imagePath, name string, args ...string) {
	t.Helper()

	command, err := debugFSCommand(name, args...)
	if err != nil {
		t.Fatalf("build debugfs command: %v", err)
	}
	if out, err := exec.Command(debugfsBinary, "-w", "-R", command, imagePath).CombinedOutput(); err != nil {
		t.Fatalf("debugfs %s: %v (%s)", name, err, string(out))
	}
}
