package main

import (
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestHandleExecStreamsOutputAndExit(t *testing.T) {
	stdout, stderr, res := runExecRequest(t, vsockexec.ExecRequest{
		Command: []string{"/bin/sh", "-c", "printf out; printf err >&2"},
	}, nil)

	if got, want := stdout, "out"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr, "err"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if got, want := res.ExitCode, 0; got != want {
		t.Fatalf("exit code = %d, want %d", got, want)
	}
	if res.Error != "" {
		t.Fatalf("unexpected error: %q", res.Error)
	}
}

func TestHandleExecUsesDirEnvAndStdin(t *testing.T) {
	dir := t.TempDir()
	wantDir := canonicalPath(t, dir)
	stdout, stderr, res := runExecRequest(t, vsockexec.ExecRequest{
		Command: []string{"/bin/sh", "-c", "printf '%s:%s:' \"$PWD\" \"$CLEANROOM_TEST_VALUE\"; cat"},
		Dir:     dir,
		Env:     []string{"CLEANROOM_TEST_VALUE=ok"},
	}, []byte("input"))

	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if got, want := stdout, wantDir+":ok:input"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := res.ExitCode, 0; got != want {
		t.Fatalf("exit code = %d, want %d", got, want)
	}
}

func TestHandleExecReportsNonZeroExit(t *testing.T) {
	stdout, stderr, res := runExecRequest(t, vsockexec.ExecRequest{
		Command: []string{"/bin/sh", "-c", "printf bad >&2; exit 7"},
	}, nil)

	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if got, want := stderr, "bad"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if got, want := res.ExitCode, 7; got != want {
		t.Fatalf("exit code = %d, want %d", got, want)
	}
	if res.Error != "" {
		t.Fatalf("unexpected error: %q", res.Error)
	}
}

func TestHandleExecReportsSignaledExit(t *testing.T) {
	stdout, stderr, res := runExecRequest(t, vsockexec.ExecRequest{
		Command: []string{"/bin/sh", "-c", "kill -TERM $$"},
	}, nil)

	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if got, want := res.ExitCode, 143; got != want {
		t.Fatalf("exit code = %d, want %d", got, want)
	}
	if res.Error == "" {
		t.Fatal("expected signal error")
	}
}

func TestHandleExecAcceptsProbeRequestShape(t *testing.T) {
	client, done := startTestAgent(t)
	defer client.Close()

	dir := t.TempDir()
	wantDir := canonicalPath(t, dir)
	if err := json.NewEncoder(client).Encode(map[string]any{
		"type":              "exec",
		"command":           []string{"/bin/sh", "-c", "printf '%s:%s' \"$PWD\" \"$CLEANROOM_TEST_VALUE\""},
		"environment":       map[string]string{"CLEANROOM_TEST_VALUE": "ok"},
		"working_directory": dir,
	}); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	if err := vsockexec.EncodeInputFrame(client, vsockexec.ExecInputFrame{Type: "eof"}); err != nil {
		t.Fatalf("encode eof: %v", err)
	}

	var stdout strings.Builder
	res, err := vsockexec.DecodeStreamResponse(client, vsockexec.StreamCallbacks{
		OnStdout: func(chunk []byte) { stdout.Write(chunk) },
	})
	if err != nil {
		t.Fatalf("decode stream response: %v", err)
	}
	if got, want := stdout.String(), wantDir+":ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := res.ExitCode, 0; got != want {
		t.Fatalf("exit code = %d, want %d", got, want)
	}

	client.Close()
	<-done
}

func TestHandleVersionRequest(t *testing.T) {
	client, done := startTestAgent(t)
	defer client.Close()

	if err := json.NewEncoder(client).Encode(map[string]string{"type": "version"}); err != nil {
		t.Fatalf("encode request: %v", err)
	}

	var res controlResponse
	if err := json.NewDecoder(client).Decode(&res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got, want := res.Type, "version"; got != want {
		t.Fatalf("type = %q, want %q", got, want)
	}
	if got, want := res.Version, agentVersion; got != want {
		t.Fatalf("version = %q, want %q", got, want)
	}
	if !contains(res.Capabilities, "exec") {
		t.Fatalf("capabilities = %#v, want exec", res.Capabilities)
	}

	client.Close()
	<-done
}

func TestDefaultPortFromEnv(t *testing.T) {
	t.Setenv("CLEANROOM_VSOCK_PORT", "12000")
	port, err := defaultPortFromEnv()
	if err != nil {
		t.Fatalf("defaultPortFromEnv returned error: %v", err)
	}
	if got, want := port, uint32(12000); got != want {
		t.Fatalf("port = %d, want %d", got, want)
	}
}

func TestBuildCommandEnvClosedEnvUsesMacOSDefaults(t *testing.T) {
	t.Setenv("HOME", "/ambient")
	env := buildCommandEnv([]string{"EXAMPLE=value"}, false)
	got := envMap(env)
	if got["HOME"] != defaultHome {
		t.Fatalf("HOME = %q, want %q", got["HOME"], defaultHome)
	}
	if got["PATH"] != defaultPath {
		t.Fatalf("PATH = %q, want %q", got["PATH"], defaultPath)
	}
	if got["EXAMPLE"] != "value" {
		t.Fatalf("EXAMPLE = %q, want value", got["EXAMPLE"])
	}
}

func runExecRequest(t *testing.T, req vsockexec.ExecRequest, stdin []byte) (string, string, vsockexec.ExecResponse) {
	t.Helper()

	client, done := startTestAgent(t)
	defer client.Close()

	if err := vsockexec.EncodeRequest(client, req); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	if len(stdin) > 0 {
		if err := vsockexec.EncodeInputFrame(client, vsockexec.ExecInputFrame{Type: "stdin", Data: stdin}); err != nil {
			t.Fatalf("encode stdin: %v", err)
		}
	}
	if err := vsockexec.EncodeInputFrame(client, vsockexec.ExecInputFrame{Type: "eof"}); err != nil {
		t.Fatalf("encode eof: %v", err)
	}

	var stdout, stderr strings.Builder
	res, err := vsockexec.DecodeStreamResponse(client, vsockexec.StreamCallbacks{
		OnStdout: func(chunk []byte) { stdout.Write(chunk) },
		OnStderr: func(chunk []byte) { stderr.Write(chunk) },
	})
	if err != nil {
		t.Fatalf("decode stream response: %v", err)
	}
	client.Close()
	<-done
	return stdout.String(), stderr.String(), res
}

func startTestAgent(t *testing.T) (net.Conn, <-chan struct{}) {
	t.Helper()

	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleConn(server)
	}()
	return client, done
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func envMap(entries []string) map[string]string {
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			out[entry] = ""
			continue
		}
		out[key] = value
	}
	return out
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	return resolved
}
