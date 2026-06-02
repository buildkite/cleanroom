//go:build darwin

package main

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

const darwinVsockCIDAny = 0xffffffff

type vsockListener struct {
	fd     int
	closed atomic.Bool
}

func listenVsock(port uint32) (streamListener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("open vsock listener: %w", err)
	}
	unix.CloseOnExec(fd)
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: darwinVsockCIDAny, Port: port}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("bind vsock port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("listen vsock port %d: %w", port, err)
	}
	return &vsockListener{fd: fd}, nil
}

func (l *vsockListener) Accept() (io.ReadWriteCloser, error) {
	for {
		fd, _, err := unix.Accept(l.fd)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			if l.closed.Load() {
				return nil, errListenerClosed
			}
			return nil, err
		}
		unix.CloseOnExec(fd)
		return os.NewFile(uintptr(fd), "cleanroom-macos-guest-agent-vsock"), nil
	}
}

func (l *vsockListener) Close() error {
	if l.closed.Swap(true) {
		return nil
	}
	if err := unix.Close(l.fd); err != nil {
		return fmt.Errorf("close vsock listener: %w", err)
	}
	return nil
}
