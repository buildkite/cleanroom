package controlserver

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestWriteSandboxPortDataHandlesShortWrites(t *testing.T) {
	conn := &shortWriteConn{maxWrite: 2}

	if err := writeSandboxPortData(conn, []byte("abcdef")); err != nil {
		t.Fatalf("writeSandboxPortData returned error: %v", err)
	}
	if got, want := conn.buf.String(), "abcdef"; got != want {
		t.Fatalf("unexpected written data: got %q want %q", got, want)
	}
	if got, want := conn.writeCalls, 3; got != want {
		t.Fatalf("unexpected write calls: got %d want %d", got, want)
	}
}

func TestWriteSandboxPortDataRejectsZeroLengthWrite(t *testing.T) {
	conn := &shortWriteConn{}

	err := writeSandboxPortData(conn, []byte("abc"))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected ErrShortWrite, got %v", err)
	}
}

type shortWriteConn struct {
	buf        bytes.Buffer
	maxWrite   int
	writeCalls int
}

func (c *shortWriteConn) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (c *shortWriteConn) Write(p []byte) (int, error) {
	c.writeCalls++
	if c.maxWrite <= 0 {
		return 0, nil
	}
	if len(p) > c.maxWrite {
		p = p[:c.maxWrite]
	}
	return c.buf.Write(p)
}

func (c *shortWriteConn) Close() error {
	return nil
}

func (c *shortWriteConn) LocalAddr() net.Addr {
	return portTestAddr("local")
}

func (c *shortWriteConn) RemoteAddr() net.Addr {
	return portTestAddr("remote")
}

func (c *shortWriteConn) SetDeadline(time.Time) error {
	return nil
}

func (c *shortWriteConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *shortWriteConn) SetWriteDeadline(time.Time) error {
	return nil
}

type portTestAddr string

func (a portTestAddr) Network() string {
	return "test"
}

func (a portTestAddr) String() string {
	return string(a)
}
