package guestexec

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

type deadlineConn interface {
	SetDeadline(time.Time) error
}

// PrepareConn applies the execution deadline when supported and closes conn
// when ctx is canceled so blocked protocol reads and writes unblock promptly.
func PrepareConn(ctx context.Context, conn io.Closer, deadlineErr string) error {
	if conn == nil {
		return nil
	}
	if dl, ok := ctx.Deadline(); ok {
		if c, ok := conn.(deadlineConn); ok {
			if err := c.SetDeadline(dl); err != nil {
				if deadlineErr == "" {
					deadlineErr = "set guest exec deadline"
				}
				return fmt.Errorf("%s: %w", deadlineErr, err)
			}
		}
	}
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	return nil
}

// SendRequest writes the guest execution request using the vsockexec protocol.
func SendRequest(w io.Writer, req vsockexec.ExecRequest) error {
	if err := vsockexec.EncodeRequest(w, req); err != nil {
		return fmt.Errorf("send guest exec request: %w", err)
	}
	return nil
}

// AttachStream exposes stdin and TTY resize writers for an execution stream.
func AttachStream(w io.Writer, stream backend.OutputStream, metadata map[string]string) {
	if stream.OnAttach == nil {
		return
	}
	inputSender := &inputFrameSender{w: w}
	stream.OnAttach(backend.AttachIO{
		WriteStdin: func(data []byte) error {
			return inputSender.Send(vsockexec.ExecInputFrame{
				Type: "stdin",
				Data: data,
			})
		},
		CloseStdin: func() error {
			return inputSender.Send(vsockexec.ExecInputFrame{Type: "eof"})
		},
		ResizeTTY: func(cols, rows uint32) error {
			return inputSender.Send(vsockexec.ExecInputFrame{
				Type: "resize",
				Cols: cols,
				Rows: rows,
			})
		},
		Metadata: metadata,
	})
}

// DecodeResponse reads stream frames into an execution response while
// forwarding stdout and stderr callbacks.
func DecodeResponse(r io.Reader, stream backend.OutputStream) (vsockexec.ExecResponse, error) {
	return vsockexec.DecodeStreamResponse(r, vsockexec.StreamCallbacks{
		OnStdout: stream.OnStdout,
		OnStderr: stream.OnStderr,
	})
}

type inputFrameSender struct {
	w  io.Writer
	mu sync.Mutex
}

func (s *inputFrameSender) Send(frame vsockexec.ExecInputFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return vsockexec.EncodeInputFrame(s.w, frame)
}
