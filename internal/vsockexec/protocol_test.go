package vsockexec

import (
	"bytes"
	"strings"
	"testing"
)

func TestDecodeRequestPreservesCommandArgumentWhitespace(t *testing.T) {
	t.Parallel()

	raw := `{"command":["head","-c","10","--","/home/sprite/artifacts/result.txt "]}`
	req, err := DecodeRequest(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("DecodeRequest returned error: %v", err)
	}
	if got, want := req.Command[4], "/home/sprite/artifacts/result.txt "; got != want {
		t.Fatalf("unexpected command arg: got %q want %q", got, want)
	}
}

func TestDecodeRequestRejectsEmptyExecutableAfterTrim(t *testing.T) {
	t.Parallel()

	raw := `{"command":["   ","echo","hi"]}`
	_, err := DecodeRequest(strings.NewReader(raw))
	if err == nil {
		t.Fatal("expected error for blank executable")
	}
}

func TestDecodeRequestWithTTY(t *testing.T) {
	t.Parallel()
	raw := `{"command":["sh"],"tty":true}`
	req, err := DecodeRequest(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("DecodeRequest returned error: %v", err)
	}
	if !req.TTY {
		t.Fatal("expected TTY to be true")
	}
}

func TestDecodeRequestTTYDefaultsFalse(t *testing.T) {
	t.Parallel()
	raw := `{"command":["echo","hi"]}`
	req, err := DecodeRequest(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("DecodeRequest returned error: %v", err)
	}
	if req.TTY {
		t.Fatal("expected TTY to default to false")
	}
}

func TestInputFrameRoundTrip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := EncodeInputFrame(&buf, ExecInputFrame{Type: "stdin", Data: []byte("hello\n")})
	if err != nil {
		t.Fatalf("EncodeInputFrame: %v", err)
	}
	frame, err := DecodeInputFrame(&buf)
	if err != nil {
		t.Fatalf("DecodeInputFrame: %v", err)
	}
	if frame.Type != "stdin" {
		t.Fatalf("expected type stdin, got %q", frame.Type)
	}
	if string(frame.Data) != "hello\n" {
		t.Fatalf("expected data %q, got %q", "hello\n", string(frame.Data))
	}
}

func TestInputFrameResize(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := EncodeInputFrame(&buf, ExecInputFrame{Type: "resize", Cols: 120, Rows: 40})
	if err != nil {
		t.Fatalf("EncodeInputFrame: %v", err)
	}
	frame, err := DecodeInputFrame(&buf)
	if err != nil {
		t.Fatalf("DecodeInputFrame: %v", err)
	}
	if frame.Type != "resize" {
		t.Fatalf("expected type resize, got %q", frame.Type)
	}
	if frame.Cols != 120 || frame.Rows != 40 {
		t.Fatalf("unexpected size: %dx%d", frame.Cols, frame.Rows)
	}
}
