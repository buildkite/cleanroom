//go:build linux

package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/vsockexec"
	"github.com/creack/pty"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

func main() {
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

	var req vsockexec.ExecRequest
	if err := dec.Decode(&req); err != nil {
		sendErrorResponse(conn, err)
		return
	}
	if len(req.Command) == 0 {
		sendErrorResponse(conn, errors.New("missing command"))
		return
	}
	if strings.TrimSpace(req.Command[0]) == "" {
		sendErrorResponse(conn, errors.New("missing command executable"))
		return
	}
	if len(req.EntropySeed) > 0 {
		_ = injectEntropy(req.EntropySeed)
	}
	if err := setupCacheOutputMountsOnce(req.CacheOutputMounts); err != nil {
		sendErrorResponse(conn, err)
		return
	}
	projectedReq, cleanupInputProjection, err := setupInputProjection(req)
	if err != nil {
		sendErrorResponse(conn, err)
		return
	}
	defer cleanupInputProjection()
	req = projectedReq

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
	env, err := buildCommandEnv(req.Env)
	if err != nil {
		sendErrorResponse(conn, err)
		return
	}
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

	sendExitResult(sender, cmd.Wait())
}

func handleConnPipes(conn io.ReadWriteCloser, dec *json.Decoder, req vsockexec.ExecRequest) {
	cmd := exec.Command(req.Command[0], req.Command[1:]...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	env, err := buildCommandEnv(req.Env)
	if err != nil {
		sendErrorResponse(conn, err)
		return
	}
	cmd.Env = env

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

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(streamFrameWriter{send: sender.Send, kind: "stdout"}, stdout)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(streamFrameWriter{send: sender.Send, kind: "stderr"}, stderr)
	}()

	wg.Wait()
	waitErr := cmd.Wait()
	exitCode, errMsg := exitResult(waitErr)

	if err := sender.Send(vsockexec.ExecStreamFrame{
		Type:     "exit",
		ExitCode: exitCode,
		Error:    errMsg,
	}); err != nil {
		return
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
	msg := err.Error()
	sender := newFrameSender(w)
	if sendErr := sender.Send(vsockexec.ExecStreamFrame{
		Type: "stderr",
		Data: []byte(msg + "\n"),
	}); sendErr != nil {
		return
	}
	if sendErr := sender.Send(vsockexec.ExecStreamFrame{
		Type:     "exit",
		ExitCode: 1,
		Error:    msg,
	}); sendErr != nil {
		return
	}
}

func sendExitResult(sender *frameSender, waitErr error) {
	exitCode, errMsg := exitResult(waitErr)
	if err := sender.Send(vsockexec.ExecStreamFrame{
		Type:     "exit",
		ExitCode: exitCode,
		Error:    errMsg,
	}); err != nil {
		return
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

func buildCommandEnv(requestEnv []string) ([]string, error) {
	// Start from the current process environment so caller-provided values can
	// override, while ensuring we have baseline HOME/PATH defaults for lookups.
	base := map[string]string{}
	requestKeys := make(map[string]struct{}, len(requestEnv))
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
		requestKeys[key] = struct{}{}
		base[key] = value
	}

	if err := configureBundlerMirror(base); err != nil {
		return nil, err
	}
	configureGatewayGoEnv(base, requestKeys)

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
	return out, nil
}

func configureGatewayGoEnv(base map[string]string, requestKeys map[string]struct{}) {
	if base == nil {
		return
	}

	if defaultProxy, ok := base[gateway.GoProxyDefaultEnvKey]; ok {
		if _, explicit := requestKeys["GOPROXY"]; !explicit && strings.TrimSpace(base["GOPROXY"]) == "" {
			base["GOPROXY"] = defaultProxy
		}
		delete(base, gateway.GoProxyDefaultEnvKey)
	}

	if defaultMirror, ok := base[gateway.MiseGoDownloadMirrorDefaultEnvKey]; ok {
		if _, explicit := requestKeys["MISE_GO_DOWNLOAD_MIRROR"]; !explicit && strings.TrimSpace(base["MISE_GO_DOWNLOAD_MIRROR"]) == "" {
			base["MISE_GO_DOWNLOAD_MIRROR"] = defaultMirror
		}
		delete(base, gateway.MiseGoDownloadMirrorDefaultEnvKey)
	}
}

func configureBundlerMirror(base map[string]string) error {
	if base == nil {
		return nil
	}

	mirror := strings.TrimSpace(base[gateway.BundlerRubyGemsMirrorEnvKey])
	if mirror == "" {
		delete(base, gateway.BundlerRubyGemsMirrorEnvKey)
		delete(base, gateway.BundlerRubyGemsFallbackTimeoutEnvKey)
		return nil
	}

	timeout := strings.TrimSpace(base[gateway.BundlerRubyGemsFallbackTimeoutEnvKey])
	configDir := strings.TrimSpace(base[gateway.BundlerAppConfigEnvKey])
	if configDir == "" {
		configDir = gateway.BundlerAppConfigPath
		base[gateway.BundlerAppConfigEnvKey] = configDir
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("create Bundler config dir: %w", err)
	}
	configContent := fmt.Sprintf(
		"---\nBUNDLE_MIRROR__HTTPS://RUBYGEMS__ORG/: %q\nBUNDLE_MIRROR__HTTPS://RUBYGEMS__ORG/__FALLBACK_TIMEOUT: %q\n",
		mirror,
		timeout,
	)
	if err := os.WriteFile(filepath.Join(configDir, "config"), []byte(configContent), 0o644); err != nil {
		return fmt.Errorf("write Bundler config: %w", err)
	}
	delete(base, gateway.BundlerRubyGemsMirrorEnvKey)
	delete(base, gateway.BundlerRubyGemsFallbackTimeoutEnvKey)
	return nil
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

func injectEntropy(entropy []byte) error {
	if len(entropy) == 0 {
		return nil
	}

	// Best effort fallback: mix caller-provided entropy into urandom even if entropy credit ioctl is unavailable.
	if err := os.WriteFile("/dev/urandom", entropy, 0o000); err != nil {
		_ = err
	}

	f, err := os.OpenFile("/dev/random", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	// Linux rand_pool_info:
	// struct rand_pool_info { int entropy_count; int buf_size; __u32 buf[0]; };
	payload := make([]byte, 8+len(entropy))
	binary.LittleEndian.PutUint32(payload[0:4], uint32(len(entropy)*8))
	binary.LittleEndian.PutUint32(payload[4:8], uint32(len(entropy)))
	copy(payload[8:], entropy)

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(unix.RNDADDENTROPY), uintptr(unsafe.Pointer(&payload[0])))
	if errno != 0 {
		return errno
	}
	return nil
}
