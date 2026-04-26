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

	"github.com/buildkite/cleanroom/internal/backend"
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

func TestCopyFromSandboxPreservesLocalDestinationSymlink(t *testing.T) {
	payload := []byte("payload")
	adapter := &copyIntegrationAdapter{
		readFn: func(_ context.Context, _ string, _ string, _ int64, emit func([]byte) error) error {
			return emit(payload)
		},
	}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createCopyTestSandbox(t, host)
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink("target.txt", link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)

	cmd := CopyCommand{
		clientFlags: clientFlags{Host: host},
		Source:      sandboxID + ":/var/log/result.txt",
		Destination: link,
	}
	if err := cmd.Run(&runtimeContext{
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("CopyCommand.Run returned error: %v", err)
	}

	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected destination to remain a symlink, got mode %s", linkInfo.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("unexpected target data: got %q want %q", data, payload)
	}
}

func TestCopyFromSandboxPreservesLocalDestinationSymlinkChain(t *testing.T) {
	payload := []byte("payload")
	adapter := &copyIntegrationAdapter{
		readFn: func(_ context.Context, _ string, _ string, _ int64, emit func([]byte) error) error {
			return emit(payload)
		},
	}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createCopyTestSandbox(t, host)
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link2 := filepath.Join(dir, "link2.txt")
	if err := os.Symlink("target.txt", link2); err != nil {
		t.Fatalf("create second symlink: %v", err)
	}
	link1 := filepath.Join(dir, "link1.txt")
	if err := os.Symlink("link2.txt", link1); err != nil {
		t.Fatalf("create first symlink: %v", err)
	}
	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)

	cmd := CopyCommand{
		clientFlags: clientFlags{Host: host},
		Source:      sandboxID + ":/var/log/result.txt",
		Destination: link1,
	}
	if err := cmd.Run(&runtimeContext{
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("CopyCommand.Run returned error: %v", err)
	}

	for _, link := range []string{link1, link2} {
		linkInfo, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("lstat %s: %v", filepath.Base(link), err)
		}
		if linkInfo.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expected %s to remain a symlink, got mode %s", filepath.Base(link), linkInfo.Mode())
		}
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("unexpected target data: got %q want %q", data, payload)
	}
}

func TestCopyFromSandboxWritesThroughDanglingLocalDestinationSymlink(t *testing.T) {
	payload := []byte("payload")
	adapter := &copyIntegrationAdapter{
		readFn: func(_ context.Context, _ string, _ string, _ int64, emit func([]byte) error) error {
			return emit(payload)
		},
	}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createCopyTestSandbox(t, host)
	dir := t.TempDir()
	target := filepath.Join(dir, "missing-target.txt")
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink("missing-target.txt", link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)

	cmd := CopyCommand{
		clientFlags: clientFlags{Host: host},
		Source:      sandboxID + ":/var/log/result.txt",
		Destination: link,
	}
	if err := cmd.Run(&runtimeContext{
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("CopyCommand.Run returned error: %v", err)
	}

	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected destination to remain a symlink, got mode %s", linkInfo.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("unexpected target data: got %q want %q", data, payload)
	}
}

func TestCopyFromSandboxRejectsLocalDestinationSymlinkCycle(t *testing.T) {
	readCalled := false
	adapter := &copyIntegrationAdapter{
		readFn: func(_ context.Context, _ string, _ string, _ int64, _ func([]byte) error) error {
			readCalled = true
			return nil
		},
	}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createCopyTestSandbox(t, host)
	dir := t.TempDir()
	link1 := filepath.Join(dir, "link1.txt")
	link2 := filepath.Join(dir, "link2.txt")
	if err := os.Symlink("link2.txt", link1); err != nil {
		t.Fatalf("create first symlink: %v", err)
	}
	if err := os.Symlink("link1.txt", link2); err != nil {
		t.Fatalf("create second symlink: %v", err)
	}
	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)

	cmd := CopyCommand{
		clientFlags: clientFlags{Host: host},
		Source:      sandboxID + ":/var/log/result.txt",
		Destination: link1,
	}
	err := cmd.Run(&runtimeContext{
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if err == nil {
		t.Fatal("expected symlink cycle error")
	}
	if !strings.Contains(err.Error(), "local destination symlink cycle") {
		t.Fatalf("unexpected error: %v", err)
	}
	if readCalled {
		t.Fatal("expected copy to fail before reading sandbox file")
	}
}

func TestResolveLocalCopyDestinationRejectsMissingSlashSuffixedPath(t *testing.T) {
	missingDir := filepath.Join(t.TempDir(), "missing") + string(filepath.Separator)

	_, err := resolveLocalCopyDestination(missingDir, "/var/log/result.txt")
	if err == nil {
		t.Fatal("expected missing slash-suffixed destination error")
	}
	if !strings.Contains(err.Error(), "ends with a path separator") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Clean(missingDir)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected destination directory not to be created, got stat err %v", statErr)
	}
}

func TestCopyToSandboxStreamsLocalFileToExecutionStdin(t *testing.T) {
	src := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(src, []byte("hello from host\n"), 0o640); err != nil {
		t.Fatalf("write source: %v", err)
	}
	mtime := time.Unix(1700000456, 0)
	if err := os.Chtimes(src, mtime, mtime); err != nil {
		t.Fatalf("set source mtime: %v", err)
	}

	var gotData []byte
	var gotPath string
	var gotMode fs.FileMode
	var gotMTime time.Time
	adapter := &copyIntegrationAdapter{
		writeFn: func(_ context.Context, _ string, path string, r io.Reader, mode fs.FileMode, mtime time.Time) (int64, error) {
			gotPath = path
			data, err := io.ReadAll(r)
			if err != nil {
				return 0, err
			}
			gotData = append([]byte(nil), data...)
			gotMode = mode
			gotMTime = mtime
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
	if got, want := gotMTime.Unix(), mtime.Unix(); got != want {
		t.Fatalf("unexpected remote mtime: got %d want %d", got, want)
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

func TestCopyToSandboxAppendsLocalBasenameForExistingRemoteDirectory(t *testing.T) {
	src := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	var gotRemotePath string
	adapter := &copyIntegrationAdapter{
		statFn: func(_ context.Context, _ string, path string) (*backend.SandboxPathInfo, error) {
			if got, want := path, "/tmp"; got != want {
				t.Fatalf("unexpected stat path: got %q want %q", got, want)
			}
			return &backend.SandboxPathInfo{Path: path, Type: backend.SandboxPathTypeDirectory}, nil
		},
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
		Destination: sandboxID + ":/tmp",
	}
	if err := cmd.Run(&runtimeContext{
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("CopyCommand.Run returned error: %v", err)
	}
	if got, want := gotRemotePath, "/tmp/fixture.txt"; got != want {
		t.Fatalf("unexpected remote path: got %q want %q", got, want)
	}
}

func TestCopyToSandboxAppendsLocalBasenameForRemoteSymlinkDirectory(t *testing.T) {
	src := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	var gotRemotePath string
	var statPaths []string
	adapter := &copyIntegrationAdapter{
		statFn: func(_ context.Context, _ string, path string) (*backend.SandboxPathInfo, error) {
			statPaths = append(statPaths, path)
			switch path {
			case "/tmp/link":
				return &backend.SandboxPathInfo{Path: path, Type: backend.SandboxPathTypeSymlink, SymlinkTarget: "artifacts"}, nil
			case "/tmp/artifacts":
				return &backend.SandboxPathInfo{Path: path, Type: backend.SandboxPathTypeDirectory}, nil
			default:
				return nil, backend.NewSandboxPathNotFoundError(path)
			}
		},
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
		Destination: sandboxID + ":/tmp/link",
	}
	if err := cmd.Run(&runtimeContext{
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("CopyCommand.Run returned error: %v", err)
	}
	if got, want := strings.Join(statPaths, ","), "/tmp/link,/tmp/artifacts"; got != want {
		t.Fatalf("unexpected stat paths: got %q want %q", got, want)
	}
	if got, want := gotRemotePath, "/tmp/link/fixture.txt"; got != want {
		t.Fatalf("unexpected remote path: got %q want %q", got, want)
	}
}

func TestCopyToSandboxAppendsLocalBasenameForRemoteSymlinkDirectoryChain(t *testing.T) {
	src := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	var gotRemotePath string
	var statPaths []string
	adapter := &copyIntegrationAdapter{
		statFn: func(_ context.Context, _ string, path string) (*backend.SandboxPathInfo, error) {
			statPaths = append(statPaths, path)
			switch path {
			case "/tmp/link1":
				return &backend.SandboxPathInfo{Path: path, Type: backend.SandboxPathTypeSymlink, SymlinkTarget: "link2"}, nil
			case "/tmp/link2":
				return &backend.SandboxPathInfo{Path: path, Type: backend.SandboxPathTypeSymlink, SymlinkTarget: "/tmp/artifacts"}, nil
			case "/tmp/artifacts":
				return &backend.SandboxPathInfo{Path: path, Type: backend.SandboxPathTypeDirectory}, nil
			default:
				return nil, backend.NewSandboxPathNotFoundError(path)
			}
		},
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
		Destination: sandboxID + ":/tmp/link1",
	}
	if err := cmd.Run(&runtimeContext{
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("CopyCommand.Run returned error: %v", err)
	}
	if got, want := strings.Join(statPaths, ","), "/tmp/link1,/tmp/link2,/tmp/artifacts"; got != want {
		t.Fatalf("unexpected stat paths: got %q want %q", got, want)
	}
	if got, want := gotRemotePath, "/tmp/link1/fixture.txt"; got != want {
		t.Fatalf("unexpected remote path: got %q want %q", got, want)
	}
}

func TestCopyToSandboxRejectsRemoteSymlinkDirectoryCycle(t *testing.T) {
	src := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	writeCalled := false
	var statPaths []string
	adapter := &copyIntegrationAdapter{
		statFn: func(_ context.Context, _ string, path string) (*backend.SandboxPathInfo, error) {
			statPaths = append(statPaths, path)
			switch path {
			case "/tmp/link1":
				return &backend.SandboxPathInfo{Path: path, Type: backend.SandboxPathTypeSymlink, SymlinkTarget: "link2"}, nil
			case "/tmp/link2":
				return &backend.SandboxPathInfo{Path: path, Type: backend.SandboxPathTypeSymlink, SymlinkTarget: "link1"}, nil
			default:
				return nil, backend.NewSandboxPathNotFoundError(path)
			}
		},
		writeFn: func(_ context.Context, _ string, _ string, _ io.Reader, _ fs.FileMode, _ time.Time) (int64, error) {
			writeCalled = true
			return 0, nil
		},
	}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createCopyTestSandbox(t, host)
	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)

	cmd := CopyCommand{
		clientFlags: clientFlags{Host: host},
		Source:      src,
		Destination: sandboxID + ":/tmp/link1",
	}
	err := cmd.Run(&runtimeContext{
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if err == nil {
		t.Fatal("expected remote symlink cycle error")
	}
	if !strings.Contains(err.Error(), "remote destination symlink cycle") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := strings.Join(statPaths, ","), "/tmp/link1,/tmp/link2"; got != want {
		t.Fatalf("unexpected stat paths: got %q want %q", got, want)
	}
	if writeCalled {
		t.Fatal("expected copy to fail before writing sandbox file")
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
	statFn     func(context.Context, string, string) (*backend.SandboxPathInfo, error)
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

func (a *copyIntegrationAdapter) StatSandboxPath(ctx context.Context, sandboxID, path string) (*backend.SandboxPathInfo, error) {
	if a.statFn != nil {
		return a.statFn(ctx, sandboxID, path)
	}
	return nil, backend.NewSandboxPathNotFoundError(path)
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
