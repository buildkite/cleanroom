package vsockexec

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
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

// TestProtocolRoundTripStdinEcho simulates a full bidirectional session over a
// net.Pipe: the "host" side sends an ExecRequest followed by stdin input frames
// and an eof, while a goroutine plays the "guest" side reading the request,
// echoing stdin data back as stdout frames, and sending an exit frame.
func TestProtocolRoundTripStdinEcho(t *testing.T) {
	t.Parallel()

	hostConn, guestConn := net.Pipe()
	defer hostConn.Close()
	defer guestConn.Close()

	// Guest side: read request, echo stdin back as stdout, exit on eof.
	var guestErr error
	var guestWg sync.WaitGroup
	guestWg.Add(1)
	go func() {
		defer guestWg.Done()
		defer guestConn.Close()

		dec := json.NewDecoder(guestConn)
		var req ExecRequest
		if err := dec.Decode(&req); err != nil {
			guestErr = err
			return
		}

		enc := json.NewEncoder(guestConn)
		for {
			var frame ExecInputFrame
			if err := dec.Decode(&frame); err != nil {
				guestErr = err
				return
			}
			if frame.Type == "eof" {
				break
			}
			if frame.Type == "stdin" && len(frame.Data) > 0 {
				if err := enc.Encode(ExecStreamFrame{Type: "stdout", Data: frame.Data}); err != nil {
					guestErr = err
					return
				}
			}
		}
		_ = enc.Encode(ExecStreamFrame{Type: "exit", ExitCode: 0})
	}()

	// Host side: send request, then write stdin concurrently with reading
	// output (net.Pipe is synchronous so both sides must be active).
	if err := EncodeRequest(hostConn, ExecRequest{Command: []string{"cat"}}); err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	go func() {
		_ = EncodeInputFrame(hostConn, ExecInputFrame{Type: "stdin", Data: []byte("hello")})
		_ = EncodeInputFrame(hostConn, ExecInputFrame{Type: "stdin", Data: []byte(" world")})
		_ = EncodeInputFrame(hostConn, ExecInputFrame{Type: "eof"})
	}()

	var stdout bytes.Buffer
	res, err := DecodeStreamResponse(hostConn, StreamCallbacks{
		OnStdout: func(data []byte) { stdout.Write(data) },
	})
	if err != nil {
		t.Fatalf("DecodeStreamResponse: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", res.ExitCode)
	}
	if got, want := stdout.String(), "hello world"; got != want {
		t.Fatalf("streamed stdout: got %q want %q", got, want)
	}
	if got, want := res.Stdout, "hello world"; got != want {
		t.Fatalf("accumulated stdout: got %q want %q", got, want)
	}

	guestWg.Wait()
	if guestErr != nil {
		t.Fatalf("guest side error: %v", guestErr)
	}
}

// TestProtocolRoundTripResizeFrame verifies that resize frames are correctly
// transmitted alongside stdin data.
func TestProtocolRoundTripResizeFrame(t *testing.T) {
	t.Parallel()

	hostConn, guestConn := net.Pipe()
	defer hostConn.Close()
	defer guestConn.Close()

	type resizeEvent struct{ Cols, Rows uint32 }
	var resizes []resizeEvent
	var guestErr error
	var guestWg sync.WaitGroup
	guestWg.Add(1)
	go func() {
		defer guestWg.Done()
		defer guestConn.Close()

		dec := json.NewDecoder(guestConn)
		var req ExecRequest
		if err := dec.Decode(&req); err != nil {
			guestErr = err
			return
		}
		if !req.TTY {
			guestErr = errors.New("expected TTY=true")
			return
		}

		for {
			var frame ExecInputFrame
			if err := dec.Decode(&frame); err != nil {
				guestErr = err
				return
			}
			if frame.Type == "eof" {
				break
			}
			if frame.Type == "resize" {
				resizes = append(resizes, resizeEvent{frame.Cols, frame.Rows})
			}
		}
		enc := json.NewEncoder(guestConn)
		_ = enc.Encode(ExecStreamFrame{Type: "exit", ExitCode: 0})
	}()

	if err := EncodeRequest(hostConn, ExecRequest{Command: []string{"sh"}, TTY: true}); err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	go func() {
		_ = EncodeInputFrame(hostConn, ExecInputFrame{Type: "resize", Cols: 80, Rows: 24})
		_ = EncodeInputFrame(hostConn, ExecInputFrame{Type: "resize", Cols: 120, Rows: 40})
		_ = EncodeInputFrame(hostConn, ExecInputFrame{Type: "eof"})
	}()

	res, err := DecodeStreamResponse(hostConn, StreamCallbacks{})
	if err != nil {
		t.Fatalf("DecodeStreamResponse: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", res.ExitCode)
	}

	guestWg.Wait()
	if guestErr != nil {
		t.Fatalf("guest side error: %v", guestErr)
	}
	if len(resizes) != 2 {
		t.Fatalf("expected 2 resize events, got %d", len(resizes))
	}
	if resizes[0].Cols != 80 || resizes[0].Rows != 24 {
		t.Fatalf("resize[0]: got %dx%d, want 80x24", resizes[0].Cols, resizes[0].Rows)
	}
	if resizes[1].Cols != 120 || resizes[1].Rows != 40 {
		t.Fatalf("resize[1]: got %dx%d, want 120x40", resizes[1].Cols, resizes[1].Rows)
	}
}
