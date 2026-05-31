package gateway

import (
	"strings"
	"testing"
)

func TestLimitedOutputBufferTruncates(t *testing.T) {
	t.Parallel()

	buf := newLimitedOutputBuffer(8)
	if n, err := buf.Write([]byte("hello world")); err != nil || n != len("hello world") {
		t.Fatalf("Write returned n=%d err=%v", n, err)
	}

	got := buf.String()
	if !strings.Contains(got, "hello wo") {
		t.Fatalf("expected retained prefix in %q", got)
	}
	if !strings.Contains(got, "[truncated 3 bytes]") {
		t.Fatalf("expected truncation marker in %q", got)
	}
}

func TestLimitedOutputBufferSanitizesInvalidUTF8(t *testing.T) {
	t.Parallel()

	buf := newLimitedOutputBuffer(8)
	if _, err := buf.Write([]byte{'o', 0xff, 'k'}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if got, want := buf.String(), "o\uFFFDk"; got != want {
		t.Fatalf("unexpected output: got %q want %q", got, want)
	}
}
