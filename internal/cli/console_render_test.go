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

func TestAttachTTYSizeDefaultsWhenSizeIsZero(t *testing.T) {
	cols, rows := attachTTYSize(-1)
	if cols != 80 || rows != 24 {
		t.Fatalf("unexpected default tty size: got %dx%d want 80x24", cols, rows)
	}
}
