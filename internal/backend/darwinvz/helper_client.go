package darwinvz

import (
	"bytes"
	"context"
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
	"syscall"
	"time"
)

const (
	helperControlSocketName = "vz-helper.sock"
	helperProxySocketName   = "vz-proxy.sock"
)

var (
	helperInterruptWait = 2 * time.Second
	helperKillWait      = 2 * time.Second
)

type helperControlRequest struct {
	Op              string `json:"op"`
	KernelPath      string `json:"kernel_path,omitempty"`
	RootFSPath      string `json:"rootfs_path,omitempty"`
	VCPUs           int64  `json:"vcpus,omitempty"`
	MemoryMiB       int64  `json:"memory_mib,omitempty"`
	GuestPort       uint32 `json:"guest_port,omitempty"`
	LaunchSeconds   int64  `json:"launch_seconds,omitempty"`
	RunDir          string `json:"run_dir,omitempty"`
	ProxySocketPath string `json:"proxy_socket_path,omitempty"`
	ConsoleLogPath  string `json:"console_log_path,omitempty"`
	VMID            string `json:"vm_id,omitempty"`
}

type helperControlResponse struct {
	OK              bool             `json:"ok"`
	Error           string           `json:"error,omitempty"`
	VMID            string           `json:"vm_id,omitempty"`
	ProxySocketPath string           `json:"proxy_socket_path,omitempty"`
	TimingMS        map[string]int64 `json:"timing_ms,omitempty"`
}

type helperSession struct {
	cmd        *exec.Cmd
	socketPath string

	stderr bytes.Buffer
	done   chan error

	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
	mu   sync.Mutex
}

func startHelperSession(ctx context.Context, runDir string, launchSeconds int64) (*helperSession, error) {
	helperPath, err := resolveHelperBinaryPath()
	if err != nil {
		return nil, err
	}

	socketPath := filepath.Join(runDir, helperControlSocketName)
	if err := ensureUnixSocketPathFits(socketPath); err != nil {
		return nil, fmt.Errorf("helper control socket path %q is too long: %w", socketPath, err)
	}
	_ = os.Remove(socketPath)

	cmd := exec.Command(helperPath, "--socket", socketPath)
	cmd.Stdout = io.Discard

	session := &helperSession{
		cmd:        cmd,
		socketPath: socketPath,
		done:       make(chan error, 1),
	}
	cmd.Stderr = &session.stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start darwin-vz helper %q: %w", helperPath, err)
	}
	go func() {
		session.done <- cmd.Wait()
		close(session.done)
	}()

	startupCtx, cancel := context.WithTimeout(ctx, helperStartupTimeout(launchSeconds))
	defer cancel()

	conn, err := waitForHelperControlSocket(startupCtx, socketPath, session.done)
	if err != nil {
		_ = session.closeProcess()
		return nil, fmt.Errorf("connect darwin-vz helper control socket: %w", session.decorateError(err))
	}

	session.conn = conn
	session.enc = json.NewEncoder(conn)
	session.dec = json.NewDecoder(conn)
	return session, nil
}

func helperStartupTimeout(launchSeconds int64) time.Duration {
	if launchSeconds <= 0 {
		return 10 * time.Second
	}
	timeout := time.Duration(launchSeconds) * time.Second
	if timeout < 5*time.Second {
		return 5 * time.Second
	}
	return timeout
}

func waitForHelperControlSocket(ctx context.Context, socketPath string, helperDone <-chan error) (net.Conn, error) {
	dialer := net.Dialer{}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	for {
		conn, err := dialer.DialContext(ctx, "unix", socketPath)
		if err == nil {
			return conn, nil
		}

		select {
		case doneErr := <-helperDone:
			if doneErr == nil {
				return nil, errors.New("helper exited before control socket was ready")
			}
			return nil, fmt.Errorf("helper exited before control socket was ready: %w", doneErr)
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for helper control socket: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func dialUnixSocketWithRetry(ctx context.Context, socketPath string) (net.Conn, error) {
	dialer := net.Dialer{}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	for {
		conn, err := dialer.DialContext(ctx, "unix", socketPath)
		if err == nil {
			return conn, nil
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for unix socket %q: %w", socketPath, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *helperSession) request(ctx context.Context, req helperControlRequest) (helperControlResponse, error) {
	if s == nil {
		return helperControlResponse{}, errors.New("nil helper session")
	}

	deadline := time.Now().Add(10 * time.Second)
	if dl, ok := ctx.Deadline(); ok {
		deadline = dl
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.conn.SetDeadline(deadline); err != nil {
		return helperControlResponse{}, fmt.Errorf("set helper control deadline: %w", err)
	}
	defer s.conn.SetDeadline(time.Time{})

	if err := s.enc.Encode(req); err != nil {
		return helperControlResponse{}, s.decorateError(fmt.Errorf("send helper request %q: %w", req.Op, err))
	}

	var res helperControlResponse
	if err := s.dec.Decode(&res); err != nil {
		return helperControlResponse{}, s.decorateError(fmt.Errorf("decode helper response %q: %w", req.Op, err))
	}
	if !res.OK {
		msg := strings.TrimSpace(res.Error)
		if msg == "" {
			msg = "unknown helper error"
		}
		if strings.Contains(msg, "com.apple.security.virtualization") {
			msg += "; run `mise run install` to install and sign cleanroom-darwin-vz with the virtualization entitlement"
		}
		return helperControlResponse{}, s.decorateError(fmt.Errorf("helper %s failed: %s", req.Op, msg))
	}
	return res, nil
}

func (s *helperSession) close() error {
	if s == nil {
		return nil
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}

	err := s.closeProcess()
	_ = os.Remove(s.socketPath)
	return err
}

func (s *helperSession) closeProcess() error {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return nil
	}

	if err, ok := recvDoneNonBlocking(s.done); ok {
		return s.normalizeHelperExitErr(err)
	}

	_ = s.cmd.Process.Signal(os.Interrupt)
	if err, ok := recvDoneWithTimeout(s.done, helperInterruptWait); ok {
		return s.normalizeHelperExitErr(err)
	}

	_ = s.cmd.Process.Kill()
	if err, ok := recvDoneWithTimeout(s.done, helperKillWait); ok {
		return s.normalizeHelperExitErr(err)
	}
	return s.decorateError(errors.New("timed out waiting for helper process exit"))
}

func (s *helperSession) normalizeHelperExitErr(err error) error {
	if err != nil && !isExpectedInterruptExit(err) {
		return s.decorateError(fmt.Errorf("helper exited: %w", err))
	}
	return nil
}

func recvDoneNonBlocking(done <-chan error) (error, bool) {
	select {
	case err, ok := <-done:
		if !ok {
			return nil, true
		}
		return err, true
	default:
		return nil, false
	}
}

func recvDoneWithTimeout(done <-chan error, timeout time.Duration) (error, bool) {
	if timeout <= 0 {
		return recvDoneNonBlocking(done)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err, ok := <-done:
		if !ok {
			return nil, true
		}
		return err, true
	case <-timer.C:
		return nil, false
	}
}

func isExpectedInterruptExit(err error) bool {
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if waitStatus, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return waitStatus.Signaled() && waitStatus.Signal() == syscall.SIGINT
		}
	}
	return strings.Contains(err.Error(), "signal: interrupt")
}

func (s *helperSession) decorateError(err error) error {
	if err == nil {
		return nil
	}
	stderr := strings.TrimSpace(s.stderr.String())
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w (helper stderr: %s)", err, stderr)
}

func ensureUnixSocketPathFits(path string) error {
	const maxUnixSocketPath = 103
	if len(path) <= maxUnixSocketPath {
		return nil
	}
	return fmt.Errorf("max length is %d bytes, got %d", maxUnixSocketPath, len(path))
}
