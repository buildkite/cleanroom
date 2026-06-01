package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/buildkite/cleanroom/internal/vsockexec"
)

const (
	agentVersion = "0.1.0"
	defaultHome  = "/var/root"
	defaultPath  = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
)

var errListenerClosed = errors.New("listener closed")

type streamListener interface {
	Accept() (io.ReadWriteCloser, error)
	Close() error
}

type requestEnvelope struct {
	Type string `json:"type,omitempty"`
}

type probeExecRequest struct {
	Type             string            `json:"type,omitempty"`
	Command          []string          `json:"command"`
	Dir              string            `json:"dir,omitempty"`
	WorkingDirectory *string           `json:"working_directory,omitempty"`
	Env              []string          `json:"env,omitempty"`
	Environment      map[string]string `json:"environment,omitempty"`
	ClosedEnv        bool              `json:"closed_env,omitempty"`
}

type controlResponse struct {
	Type         string   `json:"type"`
	Version      string   `json:"version"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	Capabilities []string `json:"capabilities,omitempty"`
}

func main() {
	if err := run(os.Args[1:], os.Stderr, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "cleanroom-macos-guest-agent: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stderr io.Writer, stdout io.Writer) error {
	defaultPort, err := defaultPortFromEnv()
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("cleanroom-macos-guest-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Uint64("port", uint64(defaultPort), "virtio socket port to listen on")
	stdio := fs.Bool("stdio", false, "serve one request over stdin/stdout")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *showVersion {
		fmt.Fprintf(stdout, "cleanroom-macos-guest-agent %s\n", agentVersion)
		return nil
	}
	if *port == 0 || *port > math.MaxUint32 {
		return fmt.Errorf("port must be between 1 and %d", uint64(math.MaxUint32))
	}
	if *stdio {
		handleConn(stdioConn{r: os.Stdin, w: os.Stdout})
		return nil
	}

	ln, err := listenVsock(uint32(*port))
	if err != nil {
		return err
	}
	defer ln.Close()
	return serve(ln, stderr)
}

func defaultPortFromEnv() (uint32, error) {
	raw := strings.TrimSpace(os.Getenv("CLEANROOM_VSOCK_PORT"))
	if raw == "" {
		return vsockexec.DefaultPort, nil
	}
	port, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || port == 0 {
		return 0, fmt.Errorf("invalid CLEANROOM_VSOCK_PORT %q", raw)
	}
	return uint32(port), nil
}

func serve(ln streamListener, stderr io.Writer) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, errListenerClosed) {
				return nil
			}
			fmt.Fprintf(stderr, "accept: %v\n", err)
			continue
		}
		go handleConn(conn)
	}
}

func handleConn(conn io.ReadWriteCloser) {
	defer conn.Close()

	dec := json.NewDecoder(conn)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		sendErrorResponse(conn, err)
		return
	}

	var envelope requestEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		sendErrorResponse(conn, err)
		return
	}

	switch envelope.Type {
	case "ready", "version":
		sendControlResponse(conn, envelope.Type)
		return
	case "", "exec":
		req, err := decodeExecRequest(raw)
		if err != nil {
			sendErrorResponse(conn, err)
			return
		}
		handleExec(conn, dec, req)
	default:
		sendErrorResponse(conn, fmt.Errorf("unsupported request type %q", envelope.Type))
	}
}

func sendControlResponse(w io.Writer, typ string) {
	if typ == "" {
		typ = "version"
	}
	_ = json.NewEncoder(w).Encode(controlResponse{
		Type:    typ,
		Version: agentVersion,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		Capabilities: []string{
			"ready",
			"version",
			"exec",
			"stdin",
			"stdout",
			"stderr",
			"exit_code",
			"env",
			"working_directory",
		},
	})
}

func decodeExecRequest(raw json.RawMessage) (vsockexec.ExecRequest, error) {
	var req probeExecRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return vsockexec.ExecRequest{}, err
	}
	if len(req.Command) == 0 {
		return vsockexec.ExecRequest{}, errors.New("missing command")
	}
	if strings.TrimSpace(req.Command[0]) == "" {
		return vsockexec.ExecRequest{}, errors.New("missing command executable")
	}

	dir := req.Dir
	if req.WorkingDirectory != nil {
		dir = *req.WorkingDirectory
	}

	return vsockexec.ExecRequest{
		Command:   append([]string(nil), req.Command...),
		Dir:       dir,
		Env:       mergeEnv(req.Env, req.Environment),
		ClosedEnv: req.ClosedEnv,
	}, nil
}

func mergeEnv(entries []string, mapped map[string]string) []string {
	if len(mapped) == 0 {
		return append([]string(nil), entries...)
	}
	values := make(map[string]string, len(entries)+len(mapped))
	for _, entry := range entries {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	for key, value := range mapped {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

func handleExec(conn io.Writer, dec *json.Decoder, req vsockexec.ExecRequest) {
	cmd := exec.Command(req.Command[0], req.Command[1:]...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	cmd.Env = buildCommandEnv(req.Env, !req.ClosedEnv)

	stdin, err := cmd.StdinPipe()
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
	go readInputFrames(dec, stdin)

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
	sendExitResult(sender, waitErr)
}

func readInputFrames(dec *json.Decoder, stdin io.WriteCloser) {
	defer stdin.Close()
	for {
		var frame vsockexec.ExecInputFrame
		if err := dec.Decode(&frame); err != nil {
			return
		}
		switch frame.Type {
		case "stdin":
			if len(frame.Data) > 0 {
				if _, err := stdin.Write(frame.Data); err != nil {
					return
				}
			}
		case "eof":
			return
		}
	}
}

func buildCommandEnv(requestEnv []string, inheritAmbient bool) []string {
	base := map[string]string{}
	if inheritAmbient {
		for _, entry := range os.Environ() {
			key, value, _ := strings.Cut(entry, "=")
			base[key] = value
		}
	}
	if _, ok := base["HOME"]; !ok {
		base["HOME"] = defaultHome
	}
	if _, ok := base["PATH"]; !ok {
		base["PATH"] = defaultPath
	}
	for _, entry := range requestEnv {
		key, value, _ := strings.Cut(entry, "=")
		base[key] = value
	}

	keys := make([]string, 0, len(base))
	for key := range base {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+base[key])
	}
	return out
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
	_ = sender.Send(vsockexec.ExecStreamFrame{
		Type:     "exit",
		ExitCode: 1,
		Error:    msg,
	})
}

func sendExitResult(sender *frameSender, waitErr error) {
	exitCode, errMsg := exitResult(waitErr)
	_ = sender.Send(vsockexec.ExecStreamFrame{
		Type:     "exit",
		ExitCode: exitCode,
		Error:    errMsg,
	})
}

func exitResult(waitErr error) (int, string) {
	if waitErr == nil {
		return 0, ""
	}
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), status.Signal().String()
		}
		if code := exitErr.ExitCode(); code >= 0 {
			return code, ""
		}
	}
	return 1, waitErr.Error()
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
	if err := w.send(vsockexec.ExecStreamFrame{
		Type: w.kind,
		Data: append([]byte(nil), p...),
	}); err != nil {
		return 0, err
	}
	return len(p), nil
}

type stdioConn struct {
	r io.Reader
	w io.Writer
}

func (c stdioConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c stdioConn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (stdioConn) Close() error                  { return nil }
