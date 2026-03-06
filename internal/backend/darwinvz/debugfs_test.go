//go:build darwin

package darwinvz

import "testing"

func TestDebugFSCommandOutputErrorReportsMissingFile(t *testing.T) {
	t.Parallel()

	output := "debugfs 1.47.3 (8-Jul-2025)\n/sbin: File not found by ext2_lookup while looking up \"/sbin\"\n"
	got := debugFSCommandOutputError(output)
	if got == "" {
		t.Fatal("expected missing-file debugfs output to be treated as an error")
	}
}

func TestDebugFSCommandOutputErrorReportsUnknownCommand(t *testing.T) {
	t.Parallel()

	output := "debugfs 1.47.3 (8-Jul-2025)\ndebugfs: Command not found writee\n"
	got := debugFSCommandOutputError(output)
	if got == "" {
		t.Fatal("expected unknown-command debugfs output to be treated as an error")
	}
}

func TestDebugFSCommandOutputErrorIgnoresSuccessfulOutput(t *testing.T) {
	t.Parallel()

	output := "debugfs 1.47.3 (8-Jul-2025)\nInode: 531   Type: regular    Mode:  0755   Flags: 0x80000\n"
	if got := debugFSCommandOutputError(output); got != "" {
		t.Fatalf("expected empty error for successful debugfs output, got %q", got)
	}
}
