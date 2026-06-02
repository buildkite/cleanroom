//go:build darwin

package main

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"syscall"

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
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("set vsock listener nonblocking: %w", err)
	}
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
		fd := -1
		var err error
		syscall.ForkLock.RLock()
		fd, _, err = unix.Accept(l.fd)
		if err == nil {
			unix.CloseOnExec(fd)
			err = unix.SetNonblock(fd, false)
		}
		syscall.ForkLock.RUnlock()
		if err == unix.EINTR {
			continue
		}
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			if l.closed.Load() {
				return nil, errListenerClosed
			}
			if err := l.waitReadable(); err != nil {
				if l.closed.Load() {
					return nil, errListenerClosed
				}
				return nil, err
			}
			continue
		}
		if err != nil {
			if fd >= 0 {
				_ = unix.Close(fd)
			}
			if l.closed.Load() {
				return nil, errListenerClosed
			}
			return nil, err
		}
		return os.NewFile(uintptr(fd), "cleanroom-macos-guest-agent-vsock"), nil
	}
}

func (l *vsockListener) waitReadable() error {
	for {
		_, err := unix.Poll([]unix.PollFd{{Fd: int32(l.fd), Events: unix.POLLIN}}, 1000)
		if err == unix.EINTR {
			continue
		}
		return err
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
