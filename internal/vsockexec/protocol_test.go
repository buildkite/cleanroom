package vsockexec

import (
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
