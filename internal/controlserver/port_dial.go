package controlserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"connectrpc.com/connect"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

const sandboxPortFrameSize = 32 * 1024

type closeWriter interface {
	CloseWrite() error
}

func (s *Server) DialSandboxPort(ctx context.Context, stream *connect.BidiStream[cleanroomv1.SandboxPortFrame, cleanroomv1.SandboxPortFrame]) error {
	first, err := stream.Receive()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return toConnectError(errors.New("missing sandbox port open request"))
		}
		return toConnectError(err)
	}
	open := first.GetOpen()
	if open == nil {
		return toConnectError(errors.New("first sandbox port frame must contain open"))
	}

	conn, err := s.service.DialSandboxPort(ctx, open)
	if err != nil {
		if sendErr := sendSandboxPortOpenResult(stream, err.Error()); sendErr != nil {
			return toConnectError(errors.Join(err, sendErr))
		}
		return nil
	}
	defer conn.Close()
	if err := sendSandboxPortOpenResult(stream, ""); err != nil {
		return toConnectError(err)
	}

	receiveDone := make(chan error, 1)
	go func() {
		receiveDone <- copySandboxPortStreamToConn(stream, conn)
	}()

	sendErr := copySandboxPortConnToStream(stream, conn)
	select {
	case receiveErr := <-receiveDone:
		if receiveErr != nil {
			return toConnectError(receiveErr)
		}
	default:
	}
	if sendErr != nil {
		return toConnectError(sendErr)
	}
	return nil
}

func sendSandboxPortOpenResult(stream *connect.BidiStream[cleanroomv1.SandboxPortFrame, cleanroomv1.SandboxPortFrame], msg string) error {
	return stream.Send(&cleanroomv1.SandboxPortFrame{
		Payload: &cleanroomv1.SandboxPortFrame_OpenResult{OpenResult: &cleanroomv1.SandboxPortOpenResult{
			Error: msg,
		}},
	})
}

func copySandboxPortStreamToConn(stream *connect.BidiStream[cleanroomv1.SandboxPortFrame, cleanroomv1.SandboxPortFrame], conn net.Conn) error {
	for {
		frame, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				closeWrite(conn)
				return nil
			}
			_ = conn.Close()
			return err
		}
		if frame.GetOpen() != nil {
			_ = conn.Close()
			return errors.New("sandbox port open frame must only be sent once")
		}
		if frame.GetOpenResult() != nil {
			_ = conn.Close()
			return errors.New("sandbox port open result frame must only be sent by the server")
		}
		if frame.GetCloseWrite() != nil {
			closeWrite(conn)
			return nil
		}
		data := frame.GetData()
		if len(data) == 0 {
			continue
		}
		if err := writeSandboxPortData(conn, data); err != nil {
			_ = conn.Close()
			return fmt.Errorf("write sandbox port: %w", err)
		}
	}
}

func writeSandboxPortData(conn net.Conn, data []byte) error {
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func copySandboxPortConnToStream(stream *connect.BidiStream[cleanroomv1.SandboxPortFrame, cleanroomv1.SandboxPortFrame], conn net.Conn) error {
	buf := make([]byte, sandboxPortFrameSize)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if sendErr := stream.Send(&cleanroomv1.SandboxPortFrame{
				Payload: &cleanroomv1.SandboxPortFrame_Data{Data: chunk},
			}); sendErr != nil {
				return sendErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return stream.Send(&cleanroomv1.SandboxPortFrame{
					Payload: &cleanroomv1.SandboxPortFrame_CloseWrite{CloseWrite: &cleanroomv1.SandboxPortCloseWrite{}},
				})
			}
			return fmt.Errorf("read sandbox port: %w", err)
		}
	}
}

func closeWrite(conn net.Conn) {
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
}
