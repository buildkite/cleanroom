package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

func TestCopyCommandParsesAlias(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"cp", "local.txt", "cr_123:/tmp/local.txt"}); err != nil {
		t.Fatalf("parse cp returned error: %v", err)
	}
	if got, want := c.Copy.Source, "local.txt"; got != want {
		t.Fatalf("unexpected source: got %q want %q", got, want)
	}
	if got, want := c.Copy.Destination, "cr_123:/tmp/local.txt"; got != want {
		t.Fatalf("unexpected destination: got %q want %q", got, want)
	}
}

func TestParseCopyOperandRemote(t *testing.T) {
	operand, err := parseCopyOperand("cr_123:/tmp/result.txt ")
	if err != nil {
		t.Fatalf("parseCopyOperand returned error: %v", err)
	}
	if operand.remote == nil {
		t.Fatal("expected remote operand")
	}
	if got, want := operand.remote.sandboxID, "cr_123"; got != want {
		t.Fatalf("unexpected sandbox id: got %q want %q", got, want)
	}
	if got, want := operand.remote.path, "/tmp/result.txt "; got != want {
		t.Fatalf("unexpected remote path: got %q want %q", got, want)
	}
}

func TestCopyCommandRejectsLocalToLocal(t *testing.T) {
	cmd := CopyCommand{Source: "a.txt", Destination: "b.txt"}
	err := cmd.Run(&runtimeContext{Config: runtimeconfig.Config{}, Observability: newTestObservability(t)})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "one operand must be") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCopyCommandRejectsRemoteToRemote(t *testing.T) {
	cmd := CopyCommand{Source: "cr_1:/a", Destination: "cr_2:/b"}
	err := cmd.Run(&runtimeContext{Config: runtimeconfig.Config{}, Observability: newTestObservability(t)})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "between sandboxes") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCopyFromSandboxWritesLocalFile(t *testing.T) {
	payload := []byte{0, 1, 'h', 'i', '\n'}
	adapter := &copyIntegrationAdapter{
		readFn: func(_ context.Context, sandboxID, path string, maxBytes int64, emit func([]byte) error) error {
			if strings.TrimSpace(sandboxID) == "" {
				t.Fatal("expected sandbox id")
			}
			if got, want := path, "/tmp/artifact.bin"; got != want {
				t.Fatalf("unexpected download path: got %q want %q", got, want)
			}
			if got, want := maxBytes, int64(123); got != want {
				t.Fatalf("unexpected max bytes: got %d want %d", got, want)
			}
			return emit(payload)
		},
	}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createCopyTestSandbox(t, host)
	dest := filepath.Join(t.TempDir(), "artifact.bin")
	stdout, _ := makeStdoutCapture(t)
	stderr, stderrText := makeStdoutCapture(t)

	cmd := CopyCommand{
		clientFlags: clientFlags{Host: host},
		Source:      sandboxID + ":/tmp/artifact.bin",
		Destination: dest,
		MaxBytes:    123,
	}
	if err := cmd.Run(&runtimeContext{
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("CopyCommand.Run returned error: %v (stderr=%q)", err, stderrText())
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("unexpected copied data: got %v want %v", data, payload)
	}
}

func TestCopyFromSandboxUsesRemoteBasenameForLocalDirectory(t *testing.T) {
	adapter := &copyIntegrationAdapter{
		readFn: func(_ context.Context, _ string, _ string, _ int64, emit func([]byte) error) error {
			return emit([]byte("payload"))
		},
	}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createCopyTestSandbox(t, host)
	dir := t.TempDir()
	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)

	cmd := CopyCommand{
		clientFlags: clientFlags{Host: host},
		Source:      sandboxID + ":/var/log/result.txt",
		Destination: dir,
	}
	if err := cmd.Run(&runtimeContext{
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("CopyCommand.Run returned error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "result.txt"))
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if got, want := string(data), "payload"; got != want {
		t.Fatalf("unexpected copied data: got %q want %q", got, want)
	}
}

func TestCopyToSandboxStreamsLocalFileToExecutionStdin(t *testing.T) {
	src := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(src, []byte("hello from host\n"), 0o640); err != nil {
		t.Fatalf("write source: %v", err)
	}

	var gotData []byte
	var gotPath string
	var gotMode fs.FileMode
	adapter := &copyIntegrationAdapter{
		writeFn: func(_ context.Context, _ string, path string, r io.Reader, mode fs.FileMode, _ time.Time) (int64, error) {
			gotPath = path
			data, err := io.ReadAll(r)
			if err != nil {
				return 0, err
			}
			gotData = append([]byte(nil), data...)
			gotMode = mode
			return int64(len(data)), nil
		},
	}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createCopyTestSandbox(t, host)
	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)

	cmd := CopyCommand{
		clientFlags: clientFlags{Host: host},
		Source:      src,
		Destination: sandboxID + ":/tmp/copied.txt",
	}
	if err := cmd.Run(&runtimeContext{
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("CopyCommand.Run returned error: %v", err)
	}

	if got, want := string(gotData), "hello from host\n"; got != want {
		t.Fatalf("unexpected upload data: got %q want %q", got, want)
	}
	if got, want := gotPath, "/tmp/copied.txt"; got != want {
		t.Fatalf("unexpected remote path: got %q want %q", got, want)
	}
	if got, want := gotMode, fs.FileMode(0o640); got != want {
		t.Fatalf("unexpected remote mode: got %04o want %04o", got, want)
	}
}

func TestCopyToSandboxAppendsLocalBasenameForRemoteDirectory(t *testing.T) {
	src := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	var gotRemotePath string
	adapter := &copyIntegrationAdapter{
		writeFn: func(_ context.Context, _ string, path string, r io.Reader, _ fs.FileMode, _ time.Time) (int64, error) {
			gotRemotePath = path
			data, err := io.ReadAll(r)
			return int64(len(data)), err
		},
	}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createCopyTestSandbox(t, host)
	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)

	cmd := CopyCommand{
		clientFlags: clientFlags{Host: host},
		Source:      src,
		Destination: sandboxID + ":/tmp/artifacts/",
	}
	if err := cmd.Run(&runtimeContext{
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("CopyCommand.Run returned error: %v", err)
	}
	if got, want := gotRemotePath, "/tmp/artifacts/fixture.txt"; got != want {
		t.Fatalf("unexpected remote path: got %q want %q", got, want)
	}
}

func TestCopyFromSandboxReturnsDownloadError(t *testing.T) {
	adapter := &copyIntegrationAdapter{
		readFn: func(context.Context, string, string, int64, func([]byte) error) error {
			return errors.New("cat: missing")
		},
	}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createCopyTestSandbox(t, host)
	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)

	cmd := CopyCommand{
		clientFlags: clientFlags{Host: host},
		Source:      sandboxID + ":/tmp/missing.txt",
		Destination: filepath.Join(t.TempDir(), "missing.txt"),
	}
	err := cmd.Run(&runtimeContext{
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "read sandbox file") || !strings.Contains(err.Error(), "cat: missing") {
		t.Fatalf("unexpected error: %v", err)
	}
}

type copyIntegrationAdapter struct {
	integrationAdapter
	downloadFn func(context.Context, string, string, int64) ([]byte, error)
	uploadFn   func(context.Context, string, string, []byte, fs.FileMode) error
	readFn     func(context.Context, string, string, int64, func([]byte) error) error
	writeFn    func(context.Context, string, string, io.Reader, fs.FileMode, time.Time) (int64, error)
}

func (a *copyIntegrationAdapter) DownloadSandboxFile(ctx context.Context, sandboxID, path string, maxBytes int64) ([]byte, error) {
	if a.downloadFn != nil {
		return a.downloadFn(ctx, sandboxID, path, maxBytes)
	}
	return nil, errors.New("download not configured")
}

func (a *copyIntegrationAdapter) UploadSandboxFile(ctx context.Context, sandboxID, path string, data []byte, mode fs.FileMode) error {
	if a.uploadFn != nil {
		return a.uploadFn(ctx, sandboxID, path, data, mode)
	}
	return errors.New("upload not configured")
}

func (a *copyIntegrationAdapter) ReadSandboxFile(ctx context.Context, sandboxID, path string, maxBytes int64, emit func([]byte) error) error {
	if a.readFn != nil {
		return a.readFn(ctx, sandboxID, path, maxBytes, emit)
	}
	return errors.New("read not configured")
}

func (a *copyIntegrationAdapter) WriteSandboxFile(ctx context.Context, sandboxID, path string, r io.Reader, mode fs.FileMode, mtime time.Time) (int64, error) {
	if a.writeFn != nil {
		return a.writeFn(ctx, sandboxID, path, r, mode, mtime)
	}
	return 0, errors.New("write not configured")
}

func createCopyTestSandbox(t *testing.T, host string) string {
	t.Helper()

	ep, err := endpoint.Resolve(host)
	if err != nil {
		t.Fatalf("resolve host: %v", err)
	}
	client, err := controlclient.New(ep)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	resp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy:  copyTestPolicy(),
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	sandboxID := strings.TrimSpace(resp.GetSandbox().GetSandboxId())
	if sandboxID == "" {
		t.Fatal("create sandbox response missing sandbox id")
	}
	return sandboxID
}

func copyTestPolicy() *cleanroomv1.Policy {
	return &cleanroomv1.Policy{
		Version:        1,
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
	}
}
