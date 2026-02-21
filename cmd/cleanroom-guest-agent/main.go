//go:build linux

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/buildkite/cleanroom/internal/vsockexec"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

func main() {
	port := vsockexec.DefaultPort
	if raw := os.Getenv("CLEANROOM_VSOCK_PORT"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid CLEANROOM_VSOCK_PORT %q: %v\n", raw, err)
			os.Exit(2)
		}
		port = uint32(parsed)
	}

	ln, err := listenVsock(port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen vsock: %v\n", err)
		os.Exit(1)
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errorsIsClosed(err) {
				return
			}
			fmt.Fprintf(os.Stderr, "accept: %v\n", err)
			continue
		}
		handleConn(conn)
	}
}

func handleConn(conn net.Conn) {
	defer conn.Close()

	req, err := vsockexec.DecodeRequest(conn)
	if err != nil {
		_ = vsockexec.EncodeResponse(conn, vsockexec.ExecResponse{ExitCode: 1, Error: err.Error()})
		return
	}
	if len(req.EntropySeed) > 0 {
		_ = injectEntropy(req.EntropySeed)
	}

	cmd := exec.Command(req.Command[0], req.Command[1:]...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	cmd.Env = buildCommandEnv(req.Env)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = vsockexec.EncodeResponse(conn, vsockexec.ExecResponse{ExitCode: 1, Error: err.Error()})
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = vsockexec.EncodeResponse(conn, vsockexec.ExecResponse{ExitCode: 1, Error: err.Error()})
		return
	}

	if err := cmd.Start(); err != nil {
		_ = vsockexec.EncodeResponse(conn, vsockexec.ExecResponse{ExitCode: 1, Error: err.Error()})
		return
	}

	frameSender := newFrameSender(conn)

	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	copyErrCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := io.Copy(io.MultiWriter(&stdoutBuf, streamFrameWriter{
			send: frameSender.Send,
			kind: "stdout",
		}), stdout)
		copyErrCh <- err
	}()
	go func() {
		defer wg.Done()
		_, err := io.Copy(io.MultiWriter(&stderrBuf, streamFrameWriter{
			send: frameSender.Send,
			kind: "stderr",
		}), stderr)
		copyErrCh <- err
	}()

	// Wait for pipe readers to drain before cmd.Wait(), which closes the
	// pipes. Go docs: "It is incorrect to call Wait before all reads from
	// the pipe have completed."
	wg.Wait()
	close(copyErrCh)

	waitErr := cmd.Wait()
	for copyErr := range copyErrCh {
		if copyErr != nil && waitErr == nil {
			waitErr = copyErr
		}
	}

	res := vsockexec.ExecResponse{
		Stdout: stdoutBuf.String(),
		Stderr: stderrBuf.String(),
	}
	if waitErr == nil {
		res.ExitCode = 0
	} else if exitErr, ok := waitErr.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
	} else {
		res.ExitCode = 1
		res.Error = waitErr.Error()
	}

	if err := frameSender.Send(vsockexec.ExecStreamFrame{
		Type:     "exit",
		ExitCode: res.ExitCode,
		Error:    res.Error,
	}); err != nil {
		// Fallback for older/newer protocol mismatches.
		_ = vsockexec.EncodeResponse(conn, res)
	}
}

type frameSender struct {
	w  io.Writer
	mu sync.Mutex
}

func newFrameSender(w io.Writer) *frameSender {
	return &frameSender{w: w}
}

func (s *frameSender) Send(frame vsockexec.ExecStreamFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return vsockexec.EncodeStreamFrame(s.w, frame)
}

type streamFrameWriter struct {
	send func(vsockexec.ExecStreamFrame) error
	kind string
}

func (w streamFrameWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if w.send == nil {
		return len(p), nil
	}
	err := w.send(vsockexec.ExecStreamFrame{
		Type: w.kind,
		Data: append([]byte(nil), p...),
	})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func buildCommandEnv(requestEnv []string) []string {
	// Start from the current process environment so caller-provided values can
	// override, while ensuring we have baseline HOME/PATH defaults for lookups.
	base := map[string]string{}
	for _, entry := range os.Environ() {
		parts := strings.SplitN(entry, "=", 2)
		key := parts[0]
		value := ""
		if len(parts) == 2 {
			value = parts[1]
		}
		base[key] = value
	}

	for _, entry := range requestEnv {
		parts := strings.SplitN(entry, "=", 2)
		key := parts[0]
		value := ""
		if len(parts) == 2 {
			value = parts[1]
		}
		base[key] = value
	}

	if strings.TrimSpace(base["HOME"]) == "" {
		base["HOME"] = "/root"
	}
	if strings.TrimSpace(base["PATH"]) == "" {
		base["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/root/.local/bin"
	}

	out := make([]string, 0, len(base))
	for key, value := range base {
		out = append(out, key+"="+value)
	}
	return out
}

func errorsIsClosed(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, net.ErrClosed)
}

func listenVsock(port uint32) (net.Listener, error) {
	return vsock.Listen(port, nil)
}

func injectEntropy(seed []byte) error {
	if len(seed) == 0 {
		return nil
	}

	// Best effort fallback: mix seed into urandom even if entropy credit ioctl is unavailable.
	if err := os.WriteFile("/dev/urandom", seed, 0o000); err != nil {
		_ = err
	}

	f, err := os.OpenFile("/dev/random", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	// Linux rand_pool_info:
	// struct rand_pool_info { int entropy_count; int buf_size; __u32 buf[0]; };
	payload := make([]byte, 8+len(seed))
	binary.LittleEndian.PutUint32(payload[0:4], uint32(len(seed)*8))
	binary.LittleEndian.PutUint32(payload[4:8], uint32(len(seed)))
	copy(payload[8:], seed)

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(unix.RNDADDENTROPY), uintptr(unsafe.Pointer(&payload[0])))
	if errno != 0 {
		return errno
	}
	return nil
}
