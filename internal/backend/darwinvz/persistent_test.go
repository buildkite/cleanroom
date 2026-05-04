//go:build darwin

package darwinvz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestCapabilitiesExposeSnapshotAndFileTransfer(t *testing.T) {
	t.Parallel()

	caps := backend.CapabilitiesForAdapter(New())

	if !caps[backend.CapabilitySandboxSnapshot] {
		t.Fatalf("expected %s=true", backend.CapabilitySandboxSnapshot)
	}
	if !caps[backend.CapabilitySandboxFileDownload] {
		t.Fatalf("expected %s=true", backend.CapabilitySandboxFileDownload)
	}
	if !caps[backend.CapabilitySandboxFileUpload] {
		t.Fatalf("expected %s=true", backend.CapabilitySandboxFileUpload)
	}
	if !caps[backend.CapabilityNetworkStageScopedEgress] {
		t.Fatalf("expected %s=true", backend.CapabilityNetworkStageScopedEgress)
	}
	if !caps[backend.CapabilitySandboxCacheOutputVolumes] {
		t.Fatalf("expected %s=true", backend.CapabilitySandboxCacheOutputVolumes)
	}
	if !caps[backend.CapabilitySandboxOverlayWriteCapture] {
		t.Fatalf("expected %s=true", backend.CapabilitySandboxOverlayWriteCapture)
	}
	for _, key := range []string{
		backend.CapabilitySandboxPathStat,
		backend.CapabilitySandboxTreeWalk,
		backend.CapabilitySandboxFileRead,
		backend.CapabilitySandboxFileWrite,
		backend.CapabilitySandboxPathRemove,
		backend.CapabilitySandboxArchiveRead,
		backend.CapabilitySandboxArchiveWrite,
	} {
		if !caps[key] {
			t.Fatalf("expected %s=true", key)
		}
	}
}

func TestProvisionSandboxRejectsConcurrentProvisionForSameID(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	started := make(chan struct{})
	adapter := &Adapter{
		launchSandboxVMFn: func(_ context.Context, sandboxID string, _ *policy.CompiledPolicy, _ backend.FirecrackerConfig, _ []backend.CacheOutputVolumeSpec) (*sandboxInstance, error) {
			if sandboxID != "cr-test" {
				t.Fatalf("unexpected sandbox id %q", sandboxID)
			}
			select {
			case started <- struct{}{}:
			default:
			}
			<-block
			return &sandboxInstance{SandboxID: sandboxID}, nil
		},
	}

	compiled := &policy.CompiledPolicy{
		NetworkDefault: "deny",
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- adapter.ProvisionSandbox(context.Background(), backend.ProvisionRequest{
			SandboxID: "cr-test",
			Policy:    compiled,
		})
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first provision to start")
	}

	err := adapter.ProvisionSandbox(context.Background(), backend.ProvisionRequest{
		SandboxID: "cr-test",
		Policy:    compiled,
	})
	if err == nil {
		t.Fatal("expected second provision to fail")
	}

	close(block)
	if err := <-errCh; err != nil {
		t.Fatalf("first provision returned error: %v", err)
	}
}

func TestProvisionSandboxForwardsCacheOutputVolumes(t *testing.T) {
	t.Parallel()

	specs := []backend.CacheOutputVolumeSpec{
		{
			Stage:     "dependency-volume",
			BlockName: "toolchains",
			CacheKey:  "dependency-volume:v1:toolchains",
			VolumeID:  "dependency-volume-abc123",
			DirMappings: []backend.CacheOutputDirMapping{
				{GuestPath: "/root/.local/share/mise", Subpath: "dirs/0"},
			},
			FileMappings: []backend.CacheOutputFileMapping{
				{GuestPath: "/root/.config/mise/config.toml", Subpath: "files/0", Mode: 0o600},
			},
		},
	}
	var got []backend.CacheOutputVolumeSpec
	adapter := &Adapter{
		launchSandboxVMFn: func(_ context.Context, sandboxID string, _ *policy.CompiledPolicy, _ backend.FirecrackerConfig, cacheOutputVolumes []backend.CacheOutputVolumeSpec) (*sandboxInstance, error) {
			if sandboxID != "cr-test" {
				t.Fatalf("unexpected sandbox id %q", sandboxID)
			}
			got = append([]backend.CacheOutputVolumeSpec(nil), cacheOutputVolumes...)
			return &sandboxInstance{SandboxID: sandboxID}, nil
		},
	}

	if err := adapter.ProvisionSandbox(context.Background(), backend.ProvisionRequest{
		SandboxID:          "cr-test",
		Policy:             &policy.CompiledPolicy{NetworkDefault: "deny"},
		CacheOutputVolumes: specs,
	}); err != nil {
		t.Fatalf("ProvisionSandbox returned error: %v", err)
	}
	if !reflect.DeepEqual(got, specs) {
		t.Fatalf("unexpected cache output specs: got %#v want %#v", got, specs)
	}
}

func TestRunInSandboxUsesRequestLaunchSecondsOverride(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	defer close(block)
	adapter := &Adapter{}
	adapter.executeInSandboxFn = func(bootCtx context.Context, _ context.Context, instance *sandboxInstance, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		if instance == nil || instance.SandboxID != "cr-test" {
			t.Fatalf("unexpected sandbox instance: %#v", instance)
		}
		select {
		case <-block:
			return nil, errors.New("unexpected unblock")
		case <-bootCtx.Done():
			return nil, bootCtx.Err()
		}
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID:      "cr-test",
			CommandTimeout: 1,
		},
	}

	start := time.Now()
	_, err := adapter.RunInSandbox(context.Background(), backend.ExecutionRequest{
		SandboxID:   "cr-test",
		ExecutionID: "run-timeout",
		Command:     []string{"echo", "hello"},
		Policy: &policy.CompiledPolicy{
			NetworkDefault: "deny",
		},
		FirecrackerConfig: backend.FirecrackerConfig{
			LaunchSeconds: 3,
		},
	}, backend.OutputStream{})
	if err == nil {
		t.Fatal("expected run to fail on timeout")
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Fatalf("expected launch-seconds override to increase timeout, elapsed=%s err=%v", elapsed, err)
	}
}

func TestExecuteInSandboxForwardsOverlayCapture(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join("/tmp", "cr-ov-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".sock")
	defer os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer listener.Close()

	wantCapture := &vsockexec.OverlayCapture{
		UpperDir:            "/run/cleanroom/overlay/upper",
		BaselinePaths:       []string{"/workspace"},
		DeclaredFileOutputs: []string{"/workspace/dist/result.txt"},
		IgnoredPrefixes:     []string{"/tmp"},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			t.Errorf("accept guest exec connection: %v", acceptErr)
			return
		}
		defer conn.Close()

		var req vsockexec.ExecRequest
		if decodeErr := json.NewDecoder(conn).Decode(&req); decodeErr != nil {
			t.Errorf("decode guest exec request: %v", decodeErr)
			return
		}
		if !reflect.DeepEqual(req.OverlayCapture, wantCapture) {
			t.Errorf("unexpected overlay capture request: got %#v want %#v", req.OverlayCapture, wantCapture)
		}
		if encodeErr := vsockexec.EncodeStreamFrame(conn, vsockexec.ExecStreamFrame{
			Type:     "exit",
			ExitCode: 0,
			OverlayCapture: &vsockexec.OverlayCaptureResult{
				EscapedWrites: []vsockexec.OverlayCaptureEntry{
					{Path: "/etc/profile", Kind: "write", Mode: 0o644},
				},
			},
		}); encodeErr != nil {
			t.Errorf("encode guest exec response: %v", encodeErr)
		}
	}()

	adapter := &Adapter{}
	result, err := adapter.executeInSandbox(context.Background(), context.Background(), &sandboxInstance{
		SandboxID:       "cr-test",
		ProxySocketPath: socketPath,
		Policy:          &policy.CompiledPolicy{NetworkDefault: "deny"},
		Helper:          &helperSession{},
		exitedCh:        make(chan struct{}),
	}, backend.ExecutionRequest{
		SandboxID:   "cr-test",
		ExecutionID: "run-123",
		Command:     []string{"true"},
		Policy:      &policy.CompiledPolicy{NetworkDefault: "deny"},
		OverlayCapture: &backend.OverlayCapture{
			UpperDir:            "/run/cleanroom/overlay/upper",
			BaselinePaths:       []string{"/workspace"},
			DeclaredFileOutputs: []string{"/workspace/dist/result.txt"},
			IgnoredPrefixes:     []string{"/tmp"},
		},
	}, backend.OutputStream{})
	if err != nil {
		t.Fatalf("executeInSandbox returned error: %v", err)
	}
	<-done
	if result.OverlayCapture == nil {
		t.Fatal("expected overlay capture result")
	}
	if got, want := result.OverlayCapture.EscapedWrites[0], (backend.OverlayCaptureEntry{Path: "/etc/profile", Kind: "write", Mode: 0o644}); got != want {
		t.Fatalf("unexpected escaped write: got %#v want %#v", got, want)
	}
}

func TestRunInSandboxRejectsExitedSandbox(t *testing.T) {
	t.Parallel()

	instance := &sandboxInstance{SandboxID: "cr-test", exitedCh: make(chan struct{})}
	instance.setExited(errors.New("helper crashed"))
	close(instance.exitedCh)

	adapter := &Adapter{
		sandboxes: map[string]*sandboxInstance{
			"cr-test": instance,
		},
	}

	_, err := adapter.RunInSandbox(context.Background(), backend.ExecutionRequest{
		SandboxID:   "cr-test",
		ExecutionID: "run-dead",
		Command:     []string{"true"},
		Policy:      &policy.CompiledPolicy{NetworkDefault: "deny"},
	}, backend.OutputStream{})
	if err == nil {
		t.Fatal("expected dead sandbox run to fail")
	}
	if got := err.Error(); got == "" || !bytes.Contains([]byte(got), []byte("not running")) {
		t.Fatalf("expected not running error, got %v", err)
	}
}

func TestProbeGuestExecReadyWaitsForGuestResponse(t *testing.T) {
	t.Parallel()

	socketDir, err := os.MkdirTemp("", "cr-probe-")
	if err != nil {
		t.Fatalf("create probe temp dir: %v", err)
	}
	defer os.RemoveAll(socketDir)

	socketPath := filepath.Join(socketDir, "p.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer listener.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			t.Errorf("accept probe connection: %v", acceptErr)
			return
		}
		defer conn.Close()

		var req vsockexec.ExecRequest
		decodeErr := json.NewDecoder(conn).Decode(&req)
		if decodeErr != nil {
			t.Errorf("decode probe request: %v", decodeErr)
			return
		}
		if len(req.Command) != 0 {
			t.Errorf("expected empty probe command, got %v", req.Command)
			return
		}
		if encodeErr := vsockexec.EncodeStreamFrame(conn, vsockexec.ExecStreamFrame{
			Type:     "exit",
			ExitCode: 1,
			Error:    "missing command",
		}); encodeErr != nil {
			t.Errorf("encode probe exit frame: %v", encodeErr)
		}
	}()

	if err := probeGuestExecReady(context.Background(), nil, socketPath); err != nil {
		t.Fatalf("probeGuestExecReady returned error: %v", err)
	}
	<-done
}

func TestDefaultDarwinVZE2EImageRefUsesPublishedDigest(t *testing.T) {
	t.Parallel()

	const want = "docker.io/library/alpine@sha256:a4f4213abb84c497377b8544c81b3564f313746700372ec4fe84653e4fb03805"
	if got := defaultDarwinVZE2EImageRef(); got != want {
		t.Fatalf("unexpected default e2e image ref: got %q want %q", got, want)
	}
}

func TestDownloadSandboxFileReturnsBytes(t *testing.T) {
	t.Parallel()

	adapter := &Adapter{}
	adapter.executeInSandboxFn = func(_ context.Context, _ context.Context, instance *sandboxInstance, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		if instance == nil || instance.SandboxID != "cr-test" {
			t.Fatalf("unexpected sandbox instance: %#v", instance)
		}
		if len(req.Command) != 6 {
			t.Fatalf("unexpected command: got %v", req.Command)
		}
		if got, want := req.Command[0], "sh"; got != want {
			t.Fatalf("unexpected command[0]: got %q want %q", got, want)
		}
		if got, want := req.Command[3], "cleanroom-read"; got != want {
			t.Fatalf("unexpected command[3]: got %q want %q", got, want)
		}
		if got, want := req.Command[4], "/home/sprite/artifacts/haiku.txt"; got != want {
			t.Fatalf("unexpected read path: got %q want %q", got, want)
		}
		if got, want := req.Command[5], "33"; got != want {
			t.Fatalf("unexpected read limit: got %q want %q", got, want)
		}
		if stream.OnStdout != nil {
			stream.OnStdout([]byte("hello"))
		}
		return &backend.ExecutionResult{ExitCode: 0}, nil
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID: "cr-test",
			exitedCh:  make(chan struct{}),
		},
	}

	data, err := adapter.DownloadSandboxFile(context.Background(), "cr-test", "/home/sprite/artifacts/haiku.txt", 32)
	if err != nil {
		t.Fatalf("DownloadSandboxFile returned error: %v", err)
	}
	if got, want := string(data), "hello"; got != want {
		t.Fatalf("unexpected data: got %q want %q", got, want)
	}
}

func TestDownloadSandboxFileReturnsStreamedStderrOnFailure(t *testing.T) {
	t.Parallel()

	adapter := &Adapter{}
	adapter.executeInSandboxFn = func(_ context.Context, _ context.Context, _ *sandboxInstance, _ backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		if stream.OnStderr != nil {
			stream.OnStderr([]byte("path not found: /missing\n"))
		}
		return &backend.ExecutionResult{ExitCode: 1}, nil
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID: "cr-test",
			exitedCh:  make(chan struct{}),
		},
	}

	_, err := adapter.DownloadSandboxFile(context.Background(), "cr-test", "/missing", 32)
	if err == nil {
		t.Fatal("expected download error")
	}
	if !errors.Is(err, backend.ErrSandboxPathNotFound) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWriteSandboxFileWritesPayloadModeAndMtime(t *testing.T) {
	t.Parallel()

	var got bytes.Buffer
	closed := false
	mtime := time.Unix(1700000123, 987654321)
	adapter := &Adapter{}
	adapter.executeInSandboxFn = func(_ context.Context, _ context.Context, _ *sandboxInstance, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		if got, want := req.Command[0], "sh"; got != want {
			t.Fatalf("unexpected command[0]: got %q want %q", got, want)
		}
		if got, want := req.Command[3], "cleanroom-copy"; got != want {
			t.Fatalf("unexpected command[3]: got %q want %q", got, want)
		}
		if got, want := req.Command[4], "/home/sprite/artifacts/upload.txt"; got != want {
			t.Fatalf("unexpected upload path: got %q want %q", got, want)
		}
		if got, want := req.Command[5], "0600"; got != want {
			t.Fatalf("unexpected upload mode: got %q want %q", got, want)
		}
		if got, want := req.Command[6], "1700000123"; got != want {
			t.Fatalf("unexpected upload mtime: got %q want %q", got, want)
		}
		if stream.OnAttach != nil {
			stream.OnAttach(backend.AttachIO{
				WriteStdin: func(data []byte) error {
					_, err := got.Write(data)
					return err
				},
				CloseStdin: func() error {
					closed = true
					return nil
				},
			})
		}
		return &backend.ExecutionResult{ExitCode: 0}, nil
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID: "cr-test",
			exitedCh:  make(chan struct{}),
		},
	}

	written, err := adapter.WriteSandboxFile(context.Background(), "cr-test", "/home/sprite/artifacts/upload.txt", bytes.NewReader([]byte("payload")), 0o600, mtime)
	if err != nil {
		t.Fatalf("WriteSandboxFile returned error: %v", err)
	}
	if written != int64(len("payload")) {
		t.Fatalf("unexpected written byte count: got %d want %d", written, len("payload"))
	}
	if got.String() != "payload" {
		t.Fatalf("unexpected uploaded payload: got %q", got.String())
	}
	if !closed {
		t.Fatal("expected upload stdin to be closed")
	}
}

func TestWriteSandboxFileReturnsGuestErrorWhenUploadFailsBeforeReadingPayload(t *testing.T) {
	t.Parallel()

	writeStarted := make(chan struct{}, 1)
	unblockWrite := make(chan struct{})
	defer close(unblockWrite)
	adapter := &Adapter{}
	adapter.executeInSandboxFn = func(_ context.Context, _ context.Context, _ *sandboxInstance, _ backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		if stream.OnAttach != nil {
			stream.OnAttach(backend.AttachIO{
				WriteStdin: func([]byte) error {
					select {
					case writeStarted <- struct{}{}:
					default:
					}
					<-unblockWrite
					return errors.New("stdin closed")
				},
				CloseStdin: func() error {
					return nil
				},
			})
		}
		select {
		case <-writeStarted:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for upload copy to start")
		}
		if stream.OnStderr != nil {
			stream.OnStderr([]byte("destination is a directory: /tmp/upload\n"))
		}
		return &backend.ExecutionResult{ExitCode: 1}, nil
	}
	adapter.sandboxes = map[string]*sandboxInstance{
		"cr-test": {
			SandboxID: "cr-test",
			exitedCh:  make(chan struct{}),
		},
	}

	done := make(chan error, 1)
	go func() {
		_, err := adapter.WriteSandboxFile(context.Background(), "cr-test", "/tmp/upload", bytes.NewReader([]byte("payload")), 0o644, time.Time{})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected upload error")
		}
		if !strings.Contains(err.Error(), "destination is a directory") {
			t.Fatalf("unexpected upload error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WriteSandboxFile blocked behind upload stdin copy")
	}
}

func TestTerminateSandboxRemovesRunDir(t *testing.T) {
	t.Parallel()

	runDir := t.TempDir()
	artifactPath := runDir + "/artifact.txt"
	if err := os.WriteFile(artifactPath, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	adapter := &Adapter{
		sandboxes: map[string]*sandboxInstance{
			"cr-test": {
				SandboxID: "cr-test",
				RunDir:    runDir,
			},
		},
	}

	if err := adapter.TerminateSandbox(context.Background(), "cr-test"); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
	if _, err := os.Stat(runDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected run dir to be removed, got err=%v", err)
	}
}
