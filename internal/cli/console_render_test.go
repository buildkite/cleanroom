package cli

import "testing"

func TestNormalizeLineEndingsForRawTTYConvertsLoneLF(t *testing.T) {
	got, endedCR := normalizeLineEndingsForRawTTY([]byte("warning\n/ # "), false)
	if string(got) != "warning\r\n/ # " {
		t.Fatalf("unexpected normalized output: %q", string(got))
	}
	if endedCR {
		t.Fatal("expected endedCR=false")
	}
}

func TestNormalizeLineEndingsForRawTTYPreservesCRLFAcrossChunks(t *testing.T) {
	first, endedCR := normalizeLineEndingsForRawTTY([]byte("warning\r"), false)
	if string(first) != "warning\r" {
		t.Fatalf("unexpected first chunk: %q", string(first))
	}
	if !endedCR {
		t.Fatal("expected endedCR=true after trailing carriage return")
	}

	second, endedCR := normalizeLineEndingsForRawTTY([]byte("\n/ # "), endedCR)
	if string(second) != "\n/ # " {
		t.Fatalf("unexpected second chunk: %q", string(second))
	}
	if endedCR {
		t.Fatal("expected endedCR=false after second chunk")
	}
}

func TestShouldNormalizeRawTTYOutputTrueForShells(t *testing.T) {
	for _, command := range [][]string{
		{"sh"},
		{"bash"},
		{"/bin/zsh"},
		{"ash", "-lc", "echo hi"},
	} {
		if !shouldNormalizeRawTTYOutput(command) {
			t.Fatalf("expected normalization enabled for command %v", command)
		}
	}
}

func TestShouldNormalizeRawTTYOutputFalseForTUIs(t *testing.T) {
	for _, command := range [][]string{
		{"codex"},
		{"/usr/local/bin/codex", "--yolo"},
		{"vim"},
	} {
		if shouldNormalizeRawTTYOutput(command) {
			t.Fatalf("expected normalization disabled for command %v", command)
		}
	}
}
