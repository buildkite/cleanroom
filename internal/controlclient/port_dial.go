package controlclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

type streamPortConn struct {
	stream connectBidiPortStream
	cancel context.CancelFunc

	readPipe *io.PipeReader
	writeMu  sync.Mutex
	closeMu  sync.Mutex
	closed   atomic.Bool
	once     sync.Once

	localAddr  net.Addr
	remoteAddr net.Addr
}

type connectBidiPortStream interface {
	Send(*v1.SandboxPortFrame) error
	Receive() (*v1.SandboxPortFrame, error)
	CloseRequest() error
	CloseResponse() error
}

func (c *Client) DialSandboxPort(ctx context.Context, sandboxID string, port int) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, errors.New("missing sandbox_id")
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %d out of range 1-65535", port)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	openDone := make(chan struct{})
	var openDoneOnce sync.Once
	finishOpen := func() {
		openDoneOnce.Do(func() {
			close(openDone)
		})
	}
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-openDone:
		}
	}()
	defer finishOpen()

	stream := c.sandboxClient.DialSandboxPort(streamCtx)
	if err := stream.Send(&v1.SandboxPortFrame{
		Payload: &v1.SandboxPortFrame_Open{Open: &v1.SandboxPortOpen{
			SandboxId: sandboxID,
			GuestPort: int32(port),
		}},
	}); err != nil {
		cancel()
		return nil, err
	}
	frame, err := stream.Receive()
	if err != nil {
		cancel()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	openResult := frame.GetOpenResult()
	if openResult == nil {
		cancel()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("first sandbox port response must contain open result")
	}
	if msg := strings.TrimSpace(openResult.GetError()); msg != "" {
		cancel()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New(msg)
	}
	if err := ctx.Err(); err != nil {
		cancel()
		return nil, err
	}
	finishOpen()

	reader, writer := io.Pipe()
	conn := &streamPortConn{
		stream:     stream,
		cancel:     cancel,
		readPipe:   reader,
		localAddr:  portAddr("cleanroom-client"),
		remoteAddr: portAddr(net.JoinHostPort(sandboxID, fmt.Sprintf("%d", port))),
	}
	go conn.receive(writer)
	return conn, nil
}

func (c *streamPortConn) receive(writer *io.PipeWriter) {
	defer writer.Close()
	for {
		frame, err := c.stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) || c.closed.Load() {
				return
			}
			_ = writer.CloseWithError(err)
			return
		}
		if frame.GetCloseWrite() != nil {
			return
		}
		if frame.GetOpen() != nil || frame.GetOpenResult() != nil {
			_ = writer.CloseWithError(errors.New("unexpected sandbox port control frame"))
			return
		}
		data := frame.GetData()
		if len(data) == 0 {
			continue
		}
		if _, err := writer.Write(data); err != nil {
			return
		}
	}
}

func (c *streamPortConn) Read(b []byte) (int, error) {
	return c.readPipe.Read(b)
}

func (c *streamPortConn) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	payload := append([]byte(nil), b...)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	if err := c.stream.Send(&v1.SandboxPortFrame{
		Payload: &v1.SandboxPortFrame_Data{Data: payload},
	}); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *streamPortConn) Close() error {
	var err error
	c.once.Do(func() {
		c.closed.Store(true)
		c.writeMu.Lock()
		closeReqErr := c.stream.CloseRequest()
		c.writeMu.Unlock()
		c.cancel()
		closeRespErr := c.stream.CloseResponse()
		readErr := c.readPipe.Close()
		err = errors.Join(closeReqErr, closeRespErr, readErr)
	})
	return err
}

func (c *streamPortConn) CloseWrite() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed.Load() {
		return net.ErrClosed
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.stream.Send(&v1.SandboxPortFrame{
		Payload: &v1.SandboxPortFrame_CloseWrite{CloseWrite: &v1.SandboxPortCloseWrite{}},
	}); err != nil {
		return err
	}
	return c.stream.CloseRequest()
}

func (c *streamPortConn) LocalAddr() net.Addr {
	return c.localAddr
}

func (c *streamPortConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

func (c *streamPortConn) SetDeadline(time.Time) error {
	return nil
}

func (c *streamPortConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *streamPortConn) SetWriteDeadline(time.Time) error {
	return nil
}

type portAddr string

func (a portAddr) Network() string {
	return "cleanroom-port"
}

func (a portAddr) String() string {
	return string(a)
}
