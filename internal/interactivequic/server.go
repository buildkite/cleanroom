package interactivequic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/controlservice"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/charmbracelet/log"
	"github.com/quic-go/quic-go"
)

const DefaultALPN = "cleanroom-interactive-v1"

type Service interface {
	ConsumeInteractiveSession(sessionID, token string) (*controlservice.InteractiveSession, error)
	WriteExecutionStdin(sandboxID, executionID string, data []byte) error
	ResizeExecutionTTY(sandboxID, executionID string, cols, rows uint32) error
	CancelExecution(ctx context.Context, req *cleanroomv1.CancelExecutionRequest) (*cleanroomv1.CancelExecutionResponse, error)
	SubscribeExecutionEvents(sandboxID, executionID string) ([]*cleanroomv1.ExecutionStreamEvent, <-chan *cleanroomv1.ExecutionStreamEvent, <-chan struct{}, func(), error)
}

type Server struct {
	logger  *log.Logger
	service Service

	listener *quic.Listener
	certPin  string
	alpn     string
}

func Start(ctx context.Context, listenAddr string, service Service, logger *log.Logger) (*Server, error) {
	if service == nil {
		return nil, errors.New("missing interactive service")
	}

	tlsCfg, certPin, err := selfSignedTLSConfig(DefaultALPN)
	if err != nil {
		return nil, err
	}
	listener, err := quic.ListenAddr(listenAddr, tlsCfg, &quic.Config{
		HandshakeIdleTimeout:           10 * time.Second,
		MaxIdleTimeout:                 45 * time.Second,
		KeepAlivePeriod:                15 * time.Second,
		InitialStreamReceiveWindow:     256 * 1024,
		MaxStreamReceiveWindow:         2 * 1024 * 1024,
		InitialConnectionReceiveWindow: 1 * 1024 * 1024,
		MaxConnectionReceiveWindow:     8 * 1024 * 1024,
		MaxIncomingStreams:             8,
		MaxIncomingUniStreams:          8,
	})
	if err != nil {
		return nil, err
	}

	server := &Server{
		logger:   logger,
		service:  service,
		listener: listener,
		certPin:  certPin,
		alpn:     DefaultALPN,
	}
	go server.serve(ctx)
	return server, nil
}

func (s *Server) Addr() net.Addr {
	if s == nil || s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *Server) CertPinSHA256() string {
	if s == nil {
		return ""
	}
	return s.certPin
}

func (s *Server) ALPN() string {
	if s == nil {
		return ""
	}
	return s.alpn
}

func (s *Server) Close() error {
	if s == nil || s.listener == nil {
		return nil
	}
	return s.listener.Close()
}

func (s *Server) serve(ctx context.Context) {
	for {
		conn, err := s.listener.Accept(ctx)
		if err != nil {
			if isInteractiveAcceptClosedErr(err) || ctx.Err() != nil {
				return
			}
			if s.logger != nil {
				s.logger.Warn("interactive QUIC accept failed", "error", err)
			}
			continue
		}

		go s.handleConnection(ctx, conn)
	}
}

func (s *Server) handleConnection(ctx context.Context, conn *quic.Conn) {
	gracefulClose := false
	defer func() {
		if !gracefulClose {
			_ = conn.CloseWithError(0, "")
		}
	}()

	controlStream, err := conn.AcceptStream(ctx)
	if err != nil {
		return
	}

	decoder := json.NewDecoder(controlStream)
	encoder := json.NewEncoder(controlStream)
	sendControl := newControlSender(encoder)

	var hello controlMessage
	if err := decoder.Decode(&hello); err != nil {
		_ = sendControl(controlMessage{Type: controlTypeError, Error: "invalid hello frame"})
		return
	}
	if hello.Type != controlTypeHello {
		_ = sendControl(controlMessage{Type: controlTypeError, Error: "first control frame must be hello"})
		return
	}

	session, err := s.service.ConsumeInteractiveSession(hello.SessionID, hello.SessionToken)
	if err != nil {
		_ = sendControl(controlMessage{Type: controlTypeError, Error: err.Error()})
		return
	}
	if err := s.applyInitialTTYSize(session); err != nil {
		_ = sendControl(controlMessage{Type: controlTypeError, Error: err.Error()})
		return
	}
	if err := sendControl(controlMessage{Type: controlTypeHelloAck}); err != nil {
		return
	}

	ptyStream, err := conn.OpenUniStreamSync(ctx)
	if err != nil {
		_ = sendControl(controlMessage{Type: controlTypeError, Error: "failed to open pty stream"})
		return
	}

	history, updates, done, unsubscribe, err := s.service.SubscribeExecutionEvents(session.SandboxID, session.ExecutionID)
	if err != nil {
		_ = sendControl(controlMessage{Type: controlTypeError, Error: err.Error()})
		return
	}
	defer unsubscribe()

	controlErrCh := make(chan error, 1)
	go s.readControlLoop(ctx, decoder, session, controlErrCh)

	stdinErrCh := make(chan error, 1)
	go func() {
		stdinStream, acceptErr := conn.AcceptUniStream(ctx)
		if acceptErr != nil {
			stdinErrCh <- acceptErr
			return
		}
		s.readStdinLoop(session, stdinStream, stdinErrCh)
	}()

	for _, event := range history {
		if s.forwardEventToPTY(event, ptyStream) {
			_ = ptyStream.Close()
			_ = sendControl(controlMessage{
				Type:     controlTypeExit,
				ExitCode: event.GetExit().GetExitCode(),
				Status:   event.GetExit().GetStatus().String(),
			})
			gracefulClose = true
			return
		}
	}

	for {
		select {
		case err := <-controlErrCh:
			if err == nil || errors.Is(err, io.EOF) {
				return
			}
			_ = sendControl(controlMessage{Type: controlTypeError, Error: err.Error()})
			return
		case err := <-stdinErrCh:
			if shouldFailInteractiveOnStdinErr(err) {
				if s.logger != nil {
					s.logger.Warn("interactive stdin stream failed", "session_id", session.SessionID, "error", err)
				}
				_ = sendControl(controlMessage{Type: controlTypeError, Error: err.Error()})
				return
			}
		case event, ok := <-updates:
			if !ok {
				return
			}
			if s.forwardEventToPTY(event, ptyStream) {
				_ = ptyStream.Close()
				_ = sendControl(controlMessage{
					Type:     controlTypeExit,
					ExitCode: event.GetExit().GetExitCode(),
					Status:   event.GetExit().GetStatus().String(),
				})
				gracefulClose = true
				return
			}
		case <-done:
			for {
				select {
				case event, ok := <-updates:
					if !ok {
						return
					}
					if s.forwardEventToPTY(event, ptyStream) {
						_ = ptyStream.Close()
						_ = sendControl(controlMessage{
							Type:     controlTypeExit,
							ExitCode: event.GetExit().GetExitCode(),
							Status:   event.GetExit().GetStatus().String(),
						})
						gracefulClose = true
						return
					}
				default:
					return
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) readControlLoop(ctx context.Context, decoder *json.Decoder, session *controlservice.InteractiveSession, errCh chan<- error) {
	for {
		var msg controlMessage
		if err := decoder.Decode(&msg); err != nil {
			errCh <- err
			return
		}

		switch msg.Type {
		case controlTypeResize:
			if err := s.service.ResizeExecutionTTY(session.SandboxID, session.ExecutionID, msg.Cols, msg.Rows); err != nil {
				if isIgnorableInteractiveResizeErr(err) {
					if s.logger != nil {
						s.logger.Debug(
							"ignoring unsupported interactive resize request",
							"session_id", session.SessionID,
							"sandbox_id", session.SandboxID,
							"execution_id", session.ExecutionID,
							"cols", msg.Cols,
							"rows", msg.Rows,
						)
					}
					continue
				}
				errCh <- err
				return
			}
		case controlTypeSignal:
			_, err := s.service.CancelExecution(ctx, &cleanroomv1.CancelExecutionRequest{
				SandboxId:   session.SandboxID,
				ExecutionId: session.ExecutionID,
				Signal:      msg.Signal,
			})
			if err != nil {
				errCh <- err
				return
			}
		case controlTypeClose:
			if !msg.Detach {
				_, err := s.service.CancelExecution(ctx, &cleanroomv1.CancelExecutionRequest{
					SandboxId:   session.SandboxID,
					ExecutionId: session.ExecutionID,
					Signal:      2,
				})
				if err != nil {
					errCh <- err
					return
				}
			}
			errCh <- nil
			return
		case controlTypeStdinEOF:
			// No-op for now. Guest command exits when stdin reads EOF on stream close.
		default:
			errCh <- errors.New("unsupported control frame")
			return
		}
	}
}

func (s *Server) applyInitialTTYSize(session *controlservice.InteractiveSession) error {
	if s == nil || s.service == nil || session == nil {
		return nil
	}
	if session.InitialCols == 0 || session.InitialRows == 0 {
		return nil
	}
	if err := s.service.ResizeExecutionTTY(session.SandboxID, session.ExecutionID, session.InitialCols, session.InitialRows); err != nil {
		if isIgnorableInteractiveResizeErr(err) {
			if s.logger != nil {
				s.logger.Debug(
					"ignoring unsupported initial interactive tty size",
					"session_id", session.SessionID,
					"sandbox_id", session.SandboxID,
					"execution_id", session.ExecutionID,
					"cols", session.InitialCols,
					"rows", session.InitialRows,
				)
			}
			return nil
		}
		return err
	}
	return nil
}

func isIgnorableInteractiveResizeErr(err error) bool {
	return errors.Is(err, controlservice.ErrExecutionResizeUnsupported)
}

func shouldFailInteractiveOnStdinErr(err error) bool {
	return err != nil && !errors.Is(err, io.EOF)
}

func isInteractiveAcceptClosedErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, quic.ErrServerClosed) ||
		errors.Is(err, net.ErrClosed)
}

func (s *Server) readStdinLoop(session *controlservice.InteractiveSession, stream *quic.ReceiveStream, errCh chan<- error) {
	buf := make([]byte, 4096)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			if writeErr := s.service.WriteExecutionStdin(session.SandboxID, session.ExecutionID, append([]byte(nil), buf[:n]...)); writeErr != nil {
				errCh <- writeErr
				return
			}
		}
		if err != nil {
			errCh <- err
			return
		}
	}
}

func (s *Server) forwardEventToPTY(event *cleanroomv1.ExecutionStreamEvent, stream *quic.SendStream) bool {
	if event == nil {
		return false
	}
	switch payload := event.Payload.(type) {
	case *cleanroomv1.ExecutionStreamEvent_Stdout:
		_, _ = stream.Write(payload.Stdout)
	case *cleanroomv1.ExecutionStreamEvent_Stderr:
		_, _ = stream.Write(payload.Stderr)
	case *cleanroomv1.ExecutionStreamEvent_Exit:
		return true
	}
	return false
}

func newControlSender(enc *json.Encoder) func(controlMessage) error {
	var mu sync.Mutex
	return func(msg controlMessage) error {
		mu.Lock()
		defer mu.Unlock()
		return enc.Encode(msg)
	}
}

type controlMessage struct {
	Type         string `json:"type"`
	SessionID    string `json:"session_id,omitempty"`
	SessionToken string `json:"session_token,omitempty"`
	Cols         uint32 `json:"cols,omitempty"`
	Rows         uint32 `json:"rows,omitempty"`
	Signal       int32  `json:"signal,omitempty"`
	Detach       bool   `json:"detach,omitempty"`
	ExitCode     int32  `json:"exit_code,omitempty"`
	Status       string `json:"status,omitempty"`
	Error        string `json:"error,omitempty"`
}

const (
	controlTypeHello    = "hello"
	controlTypeHelloAck = "hello_ack"
	controlTypeResize   = "resize"
	controlTypeSignal   = "signal"
	controlTypeStdinEOF = "stdin_eof"
	controlTypeClose    = "close"
	controlTypeExit     = "exit"
	controlTypeError    = "error"
)
