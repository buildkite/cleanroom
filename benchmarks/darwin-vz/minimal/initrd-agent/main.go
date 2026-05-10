//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

const defaultPort uint32 = 10700

type execRequest struct {
	Command   []string `json:"command"`
	Dir       string   `json:"dir,omitempty"`
	Env       []string `json:"env,omitempty"`
	ClosedEnv bool     `json:"closed_env,omitempty"`
}

type execFrame struct {
	Type          string           `json:"type,omitempty"`
	Data          []byte           `json:"data,omitempty"`
	ExitCode      int              `json:"exit_code,omitempty"`
	Error         string           `json:"error,omitempty"`
	GuestTimingMS map[string]int64 `json:"guest_timing_ms,omitempty"`
}

type timingStore struct {
	mu      sync.Mutex
	timings map[string]int64
}

type frameSender struct {
	mu sync.Mutex
	w  io.Writer
}

func main() {
	mountProc()
	prepareDev()
	timings := &timingStore{timings: map[string]int64{}}
	timings.record("guest_init_start")

	port := resolvePort(readKernelCmdline())
	listener, err := listenVsock(port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen vsock: %v\n", err)
		os.Exit(1)
	}
	defer unix.Close(listener)
	timings.record("guest_agent_listen_ready")

	for {
		fd, _, err := unix.Accept4(listener, unix.SOCK_CLOEXEC)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			fmt.Fprintf(os.Stderr, "accept: %v\n", err)
			continue
		}
		timings.recordOnce("guest_agent_first_accept")
		go handleConn(os.NewFile(uintptr(fd), "vsock-conn"), timings)
	}
}

func mountProc() {
	if err := os.MkdirAll("/proc", 0o755); err != nil {
		return
	}
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil && !errors.Is(err, unix.EBUSY) {
		fmt.Fprintf(os.Stderr, "mount proc: %v\n", err)
	}
}

func prepareDev() {
	if err := os.MkdirAll("/dev", 0o755); err != nil {
		return
	}
	if err := unix.Mknod("/dev/null", unix.S_IFCHR|0o666, int(unix.Mkdev(1, 3))); err != nil && !errors.Is(err, unix.EEXIST) {
		fmt.Fprintf(os.Stderr, "mknod /dev/null: %v\n", err)
	}
}

func readKernelCmdline() string {
	raw, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func resolvePort(cmdline string) uint32 {
	if raw, ok := kernelCmdlineValue(cmdline, "cleanroom_guest_port"); ok {
		if parsed, err := strconv.ParseUint(raw, 10, 32); err == nil && parsed > 0 {
			return uint32(parsed)
		}
	}
	return defaultPort
}

func kernelCmdlineValue(cmdline, key string) (string, bool) {
	prefix := key + "="
	for _, token := range strings.Fields(cmdline) {
		if strings.HasPrefix(token, prefix) {
			return strings.TrimPrefix(token, prefix), true
		}
	}
	return "", false
}

func listenVsock(port uint32) (int, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if err := unix.Listen(fd, unix.SOMAXCONN); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func handleConn(conn *os.File, timings *timingStore) {
	defer conn.Close()

	dec := json.NewDecoder(conn)
	var req execRequest
	if err := dec.Decode(&req); err != nil {
		sendError(conn, timings, err)
		return
	}
	timings.recordOnce("guest_agent_first_request_decode")
	if len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "" {
		sendError(conn, timings, errors.New("missing command"))
		return
	}

	cmd := exec.Command(req.Command[0], req.Command[1:]...)
	if strings.TrimSpace(req.Dir) != "" {
		cmd.Dir = req.Dir
	}
	if req.ClosedEnv {
		cmd.Env = req.Env
	} else {
		cmd.Env = append(os.Environ(), req.Env...)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		sendError(conn, timings, err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		sendError(conn, timings, err)
		return
	}

	timings.recordOnce("guest_command_start")
	if err := cmd.Start(); err != nil {
		sendError(conn, timings, err)
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	sender := &frameSender{w: conn}
	go copyFrame(&wg, sender, "stdout", stdout)
	go copyFrame(&wg, sender, "stderr", stderr)
	wg.Wait()

	waitErr := cmd.Wait()
	timings.recordOnce("guest_command_exit")
	sendExit(sender, timings, waitErr)
}

func copyFrame(wg *sync.WaitGroup, sender *frameSender, kind string, r io.Reader) {
	defer wg.Done()
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_ = sender.send(execFrame{
				Type: kind,
				Data: append([]byte(nil), buf[:n]...),
			})
		}
		if err != nil {
			return
		}
	}
}

func sendError(w io.Writer, timings *timingStore, err error) {
	msg := err.Error()
	sender := &frameSender{w: w}
	_ = sender.send(execFrame{Type: "stderr", Data: []byte(msg + "\n")})
	_ = sender.send(execFrame{
		Type:          "exit",
		ExitCode:      1,
		Error:         msg,
		GuestTimingMS: timings.snapshot(),
	})
}

func sendExit(sender *frameSender, timings *timingStore, waitErr error) {
	exitCode := 0
	errMsg := ""
	if waitErr != nil {
		exitCode = 1
		errMsg = waitErr.Error()
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
			errMsg = ""
		}
	}
	_ = sender.send(execFrame{
		Type:          "exit",
		ExitCode:      exitCode,
		Error:         errMsg,
		GuestTimingMS: timings.snapshot(),
	})
}

func (s *frameSender) send(frame execFrame) error {
	if s == nil || s.w == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.NewEncoder(s.w).Encode(frame)
}

func (s *timingStore) record(name string) {
	if s == nil {
		return
	}
	ms, err := uptimeMS()
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timings[name] = ms
}

func (s *timingStore) recordOnce(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	_, exists := s.timings[name]
	s.mu.Unlock()
	if exists {
		return
	}
	s.record(name)
}

func (s *timingStore) snapshot() map[string]int64 {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64, len(s.timings))
	for key, value := range s.timings {
		out[key] = value
	}
	return out
}

func uptimeMS() (int64, error) {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0, errors.New("missing uptime")
	}
	seconds, fraction, _ := strings.Cut(fields[0], ".")
	sec, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil {
		return 0, err
	}
	fraction += "000"
	ms, err := strconv.ParseInt(fraction[:3], 10, 64)
	if err != nil {
		return 0, err
	}
	return sec*1000 + ms, nil
}

func init() {
	syscall.Umask(0)
}
