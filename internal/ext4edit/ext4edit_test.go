package ext4edit

import "testing"

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
