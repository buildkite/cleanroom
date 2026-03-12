//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/buildkite/cleanroom/internal/vsockcontrol"
	"github.com/buildkite/cleanroom/internal/vsockexec"
	"github.com/creack/pty"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) >= 3 && os.Args[1] == "checkpoint" && os.Args[2] == "request" {
		checkpointRequest()
		return
	}

	if strings.EqualFold(strings.TrimSpace(os.Getenv("CLEANROOM_GUEST_TRANSPORT")), "stdio") {
		handleConn(stdioConn{})
		return
	}

	port, err := resolveGuestAgentPort(vsockexec.DefaultPort, os.Getenv("CLEANROOM_VSOCK_PORT"), readKernelCmdline())
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(2)
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

func handleConn(conn io.ReadWriteCloser) {
	defer conn.Close()

	// Use a single json.Decoder so buffered bytes from the request aren't
	// lost when reading subsequent input frames.
	dec := json.NewDecoder(conn)

	// Peek at the first JSON object to determine message type.
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		_ = vsockexec.EncodeResponse(conn, vsockexec.ExecResponse{ExitCode: 1, Error: err.Error()})
		return
	}

	// Check for quiesce message type.
	if typeRaw, ok := raw["type"]; ok {
		var msgType string
		if err := json.Unmarshal(typeRaw, &msgType); err == nil && msgType == "quiesce" {
			handleQuiesce(conn, raw)
			return
		}
	}

	// Re-decode as exec request from the raw map.
	rawBytes, err := json.Marshal(raw)
	if err != nil {
		_ = vsockexec.EncodeResponse(conn, vsockexec.ExecResponse{ExitCode: 1, Error: err.Error()})
		return
	}
	var req vsockexec.ExecRequest
	if err := json.Unmarshal(rawBytes, &req); err != nil {
		_ = vsockexec.EncodeResponse(conn, vsockexec.ExecResponse{ExitCode: 1, Error: err.Error()})
		return
	}
	if len(req.Command) == 0 {
		_ = vsockexec.EncodeResponse(conn, vsockexec.ExecResponse{ExitCode: 1, Error: "missing command"})
		return
	}
	if strings.TrimSpace(req.Command[0]) == "" {
		_ = vsockexec.EncodeResponse(conn, vsockexec.ExecResponse{ExitCode: 1, Error: "missing command executable"})
		return
	}
	if len(req.EntropySeed) > 0 {
		_ = injectEntropy(req.EntropySeed)
	}

	if req.TTY {
		handleConnTTY(conn, dec, req)
	} else {
		handleConnPipes(conn, dec, req)
	}
}

func handleConnTTY(conn io.ReadWriteCloser, dec *json.Decoder, req vsockexec.ExecRequest) {
	cmd := exec.Command(req.Command[0], req.Command[1:]...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	env := buildCommandEnv(req.Env)
	if !envHasKey(env, "TERM") {
		env = append(env, "TERM=xterm-256color")
	}
	cmd.Env = env

	ptmx, err := pty.Start(cmd)
	if err != nil {
		sendErrorResponse(conn, err)
		return
	}
	defer ptmx.Close()

	sender := newFrameSender(conn)

	go readInputFrames(dec, ptmx, func() { _ = ptmx.Close() }, func(cols, rows uint16) {
		_ = pty.Setsize(ptmx, &pty.Winsize{Cols: cols, Rows: rows})
	})

	// PTY read returns EIO when the slave side closes; ignore the error.
	_, _ = io.Copy(streamFrameWriter{send: sender.Send, kind: "stdout"}, ptmx)

	sendExitResult(sender, conn, cmd.Wait())
}

func handleConnPipes(conn io.ReadWriteCloser, dec *json.Decoder, req vsockexec.ExecRequest) {
	cmd := exec.Command(req.Command[0], req.Command[1:]...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	cmd.Env = buildCommandEnv(req.Env)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		sendErrorResponse(conn, err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		sendErrorResponse(conn, err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		sendErrorResponse(conn, err)
		return
	}

	if err := cmd.Start(); err != nil {
		sendErrorResponse(conn, err)
		return
	}

	sender := newFrameSender(conn)

	go readInputFrames(dec, stdinPipe, func() { _ = stdinPipe.Close() }, nil)

	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.MultiWriter(&stdoutBuf, streamFrameWriter{send: sender.Send, kind: "stdout"}), stdout)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.MultiWriter(&stderrBuf, streamFrameWriter{send: sender.Send, kind: "stderr"}), stderr)
	}()

	wg.Wait()
	waitErr := cmd.Wait()
	exitCode, errMsg := exitResult(waitErr)

	if err := sender.Send(vsockexec.ExecStreamFrame{
		Type:     "exit",
		ExitCode: exitCode,
		Error:    errMsg,
	}); err != nil {
		_ = vsockexec.EncodeResponse(conn, vsockexec.ExecResponse{
			ExitCode: exitCode,
			Error:    errMsg,
			Stdout:   stdoutBuf.String(),
			Stderr:   stderrBuf.String(),
		})
	}
}

func readInputFrames(dec *json.Decoder, w io.Writer, closeStdin func(), resizeFn func(cols, rows uint16)) {
	if closeStdin != nil {
		defer closeStdin()
	}
	for {
		var frame vsockexec.ExecInputFrame
		if err := dec.Decode(&frame); err != nil {
			return
		}
		switch frame.Type {
		case "stdin":
			if len(frame.Data) > 0 {
				if _, err := w.Write(frame.Data); err != nil {
					return
				}
			}
		case "eof":
			return
		case "resize":
			if resizeFn != nil && frame.Cols > 0 && frame.Rows > 0 {
				resizeFn(uint16(frame.Cols), uint16(frame.Rows))
			}
		}
	}
}

func sendErrorResponse(w io.Writer, err error) {
	_ = vsockexec.EncodeResponse(w, vsockexec.ExecResponse{ExitCode: 1, Error: err.Error()})
}

func sendExitResult(sender *frameSender, w io.Writer, waitErr error) {
	exitCode, errMsg := exitResult(waitErr)
	if err := sender.Send(vsockexec.ExecStreamFrame{
		Type:     "exit",
		ExitCode: exitCode,
		Error:    errMsg,
	}); err != nil {
		_ = vsockexec.EncodeResponse(w, vsockexec.ExecResponse{
			ExitCode: exitCode,
			Error:    errMsg,
		})
	}
}

type stdioConn struct{}

func (stdioConn) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdioConn) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdioConn) Close() error                { return nil }

func exitResult(waitErr error) (int, string) {
	if waitErr == nil {
		return 0, ""
	}
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		return exitErr.ExitCode(), ""
	}
	return 1, waitErr.Error()
}

func envHasKey(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
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

func handleQuiesce(conn io.ReadWriteCloser, raw map[string]json.RawMessage) {
	respond := func(ok bool, errMsg string) {
		_ = vsockcontrol.EncodeQuiesceResponse(conn, vsockcontrol.QuiesceResponse{OK: ok, Error: errMsg})
	}

	// Parse optional timeout.
	var timeoutSec int64 = 10
	if ts, ok := raw["timeout_seconds"]; ok {
		_ = json.Unmarshal(ts, &timeoutSec)
	}
	if timeoutSec <= 0 {
		timeoutSec = 10
	}
	timeout := time.Duration(timeoutSec) * time.Second

	// First sync.
	if err := exec.Command("sync").Run(); err != nil {
		respond(false, fmt.Sprintf("sync: %v", err))
		return
	}

	// Run quiesce hook if it exists.
	const hookPath = "/usr/local/bin/cleanroom-quiesce-hook"
	if _, err := os.Stat(hookPath); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := exec.CommandContext(ctx, hookPath).Run(); err != nil {
			respond(false, fmt.Sprintf("quiesce hook: %v", err))
			return
		}
	}

	// Second sync.
	if err := exec.Command("sync").Run(); err != nil {
		respond(false, fmt.Sprintf("sync: %v", err))
		return
	}

	respond(true, "")
}

func checkpointRequest() {
	var name string
	args := os.Args[3:]
	for i := 0; i < len(args); i++ {
		if args[i] == "--name" && i+1 < len(args) {
			name = args[i+1]
			i++
		}
	}

	conn, err := vsock.Dial(2, vsockcontrol.DefaultHostControlPort, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial host control port: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	if err := vsockcontrol.EncodeCheckpointRequest(conn, vsockcontrol.CheckpointRequest{Name: name}); err != nil {
		fmt.Fprintf(os.Stderr, "send checkpoint request: %v\n", err)
		os.Exit(1)
	}

	resp, err := vsockcontrol.DecodeCheckpointResponse(conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read checkpoint response: %v\n", err)
		os.Exit(1)
	}

	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "checkpoint error: %s\n", resp.Error)
		os.Exit(1)
	}

	if resp.Accepted {
		fmt.Println("checkpoint accepted")
	} else {
		fmt.Println("checkpoint pending")
	}
}
