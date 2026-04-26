package guestexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestPrepareConnSetsDeadlineAndClosesOnCancel(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Hour))
	defer cancel()
	conn := &fakeConn{closed: make(chan struct{})}

	if err := PrepareConn(ctx, conn, "set test deadline"); err != nil {
		t.Fatalf("PrepareConn returned error: %v", err)
	}
	if conn.deadline.IsZero() {
		t.Fatalf("expected deadline to be set")
	}

	cancel()
	select {
	case <-conn.closed:
	case <-time.After(time.Second):
		t.Fatal("expected connection to close when context is canceled")
	}
}

func TestPrepareConnWrapsDeadlineError(t *testing.T) {
	conn := &fakeConn{setDeadlineErr: errors.New("boom"), closed: make(chan struct{})}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Hour))
	defer cancel()

	err := PrepareConn(ctx, conn, "set proxy socket deadline")
	if err == nil {
		t.Fatal("expected error")
	}
	if got, want := err.Error(), "set proxy socket deadline: boom"; got != want {
		t.Fatalf("unexpected error: got %q want %q", got, want)
	}
}

func TestAttachStreamWritesInputFramesAndMetadata(t *testing.T) {
	var buf bytes.Buffer
	var attach backend.AttachIO
	AttachStream(&buf, backend.OutputStream{
		OnAttach: func(io backend.AttachIO) {
			attach = io
		},
	}, map[string]string{"network_guest_ip": "192.0.2.10"})

	if attach.WriteStdin == nil || attach.CloseStdin == nil || attach.ResizeTTY == nil {
		t.Fatalf("expected attach IO callbacks to be set")
	}
	if got, want := attach.Metadata["network_guest_ip"], "192.0.2.10"; got != want {
		t.Fatalf("unexpected metadata: got %q want %q", got, want)
	}

	if err := attach.WriteStdin([]byte("hello")); err != nil {
		t.Fatalf("WriteStdin returned error: %v", err)
	}
	if err := attach.ResizeTTY(120, 40); err != nil {
		t.Fatalf("ResizeTTY returned error: %v", err)
	}
	if err := attach.CloseStdin(); err != nil {
		t.Fatalf("CloseStdin returned error: %v", err)
	}

	dec := json.NewDecoder(&buf)
	first := vsockexec.ExecInputFrame{}
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("decode first input frame: %v", err)
	}
	if first.Type != "stdin" || string(first.Data) != "hello" {
		t.Fatalf("unexpected first input frame: %#v", first)
	}
	second := vsockexec.ExecInputFrame{}
	if err := dec.Decode(&second); err != nil {
		t.Fatalf("decode second input frame: %v", err)
	}
	if second.Type != "resize" || second.Cols != 120 || second.Rows != 40 {
		t.Fatalf("unexpected second input frame: %#v", second)
	}
	third := vsockexec.ExecInputFrame{}
	if err := dec.Decode(&third); err != nil {
		t.Fatalf("decode third input frame: %v", err)
	}
	if third.Type != "eof" {
		t.Fatalf("unexpected third input frame: %#v", third)
	}
}

func TestDecodeResponseForwardsStreamCallbacks(t *testing.T) {
	var frames bytes.Buffer
	if err := vsockexec.EncodeStreamFrame(&frames, vsockexec.ExecStreamFrame{Type: "stdout", Data: []byte("abcdef")}); err != nil {
		t.Fatalf("EncodeStreamFrame stdout: %v", err)
	}
	if err := vsockexec.EncodeStreamFrame(&frames, vsockexec.ExecStreamFrame{Type: "exit", ExitCode: 7}); err != nil {
		t.Fatalf("EncodeStreamFrame exit: %v", err)
	}

	var streamed bytes.Buffer
	res, err := DecodeResponse(&frames, backend.OutputStream{
		OnStdout: func(chunk []byte) { streamed.Write(chunk) },
	})
	if err != nil {
		t.Fatalf("DecodeResponse returned error: %v", err)
	}
	if got, want := streamed.String(), "abcdef"; got != want {
		t.Fatalf("unexpected streamed stdout: got %q want %q", got, want)
	}
	if got, want := res.ExitCode, 7; got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
}

type fakeConn struct {
	deadline       time.Time
	setDeadlineErr error
	closed         chan struct{}
}

func (c *fakeConn) SetDeadline(deadline time.Time) error {
	c.deadline = deadline
	return c.setDeadlineErr
}

func (c *fakeConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

func (c *fakeConn) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (c *fakeConn) Write(p []byte) (int, error) {
	return len(p), nil
}
