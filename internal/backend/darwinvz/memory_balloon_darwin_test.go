//go:build darwin

package darwinvz

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestDarwinVZInitialMemoryBalloonTargetMiB(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		memoryMiB int64
		want      int64
	}{
		{name: "below adaptive start", memoryMiB: 512, want: 0},
		{name: "at adaptive start", memoryMiB: 1024, want: 0},
		{name: "above adaptive start", memoryMiB: 1025, want: 1024},
		{name: "large cap", memoryMiB: 8192, want: 1024},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := darwinVZInitialMemoryBalloonTargetMiB(tt.memoryMiB); got != tt.want {
				t.Fatalf("darwinVZInitialMemoryBalloonTargetMiB(%d) = %d, want %d", tt.memoryMiB, got, tt.want)
			}
		})
	}
}

func TestWriteDarwinVZConfigIncludesInitialMemoryBalloonTarget(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "darwin-vz-config.json")
	if err := writeDarwinVZConfig(
		configPath,
		"darwin-vz",
		"/kernel",
		"/rootfs",
		"console=hvc0",
		2,
		8192,
		1024,
		10_700,
		30,
		darwinVZNetwork{Mode: darwinVZNetworkModeFileHandle},
		nil,
	); err != nil {
		t.Fatalf("writeDarwinVZConfig returned error: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg darwinVZConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if got, want := cfg.InitialMemoryBalloonTargetMiB, int64(1024); got != want {
		t.Fatalf("initial memory balloon target = %d, want %d", got, want)
	}
}

func TestDarwinVZMemoryBalloonGrowDoesNotBlindlySleep(t *testing.T) {
	t.Parallel()

	if darwinVZMemoryBalloonGrowSettle != 0 {
		t.Fatalf("darwin-vz memory balloon grow settle = %s, want no fixed settle delay", darwinVZMemoryBalloonGrowSettle)
	}
}

func TestExecuteInSandboxRestoresMemoryBalloonTargetBeforeGuestExec(t *testing.T) {
	socketDir, err := os.MkdirTemp("", "cr-balloon-")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(socketDir)
	socketPath := filepath.Join(socketDir, "proxy.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()

		var req vsockexec.ExecRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			serverErr <- err
			return
		}
		if got, want := req.Command, []string{"true"}; len(got) != len(want) || got[0] != want[0] {
			t.Errorf("unexpected guest command: got %#v want %#v", got, want)
		}
		if err := vsockexec.EncodeStreamFrame(conn, vsockexec.ExecStreamFrame{Type: "exit", ExitCode: 0}); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	var helperReqs []helperControlRequest
	adapter := &Adapter{
		helperRequestFn: func(_ context.Context, helper *helperSession, req helperControlRequest) (helperControlResponse, error) {
			if helper == nil {
				t.Fatal("expected helper session")
			}
			helperReqs = append(helperReqs, req)
			return helperControlResponse{OK: true}, nil
		},
	}

	_, err = adapter.executeInSandbox(context.Background(), context.Background(), &sandboxInstance{
		SandboxID: "cr-test",
		VMID:      "vm-test",
		FirecrackerConfig: backend.FirecrackerConfig{
			MemoryMiB: 8192,
		},
		ProxySocketPath: socketPath,
		Policy:          &policy.CompiledPolicy{NetworkDefault: "deny"},
		Helper:          &helperSession{},
		exitedCh:        make(chan struct{}),
	}, backend.ExecutionRequest{
		SandboxID:   "cr-test",
		ExecutionID: "run-123",
		Command:     []string{"true"},
		Policy:      &policy.CompiledPolicy{NetworkDefault: "deny"},
	}, backend.OutputStream{})
	if err != nil {
		t.Fatalf("executeInSandbox returned error: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("guest exec server returned error: %v", err)
	}
	if len(helperReqs) != 1 {
		t.Fatalf("helper requests = %#v, want one SetMemoryBalloonTarget request", helperReqs)
	}
	req := helperReqs[0]
	if req.Op != "SetMemoryBalloonTarget" {
		t.Fatalf("helper op = %q, want SetMemoryBalloonTarget", req.Op)
	}
	if req.VMID != "vm-test" {
		t.Fatalf("helper vm_id = %q, want vm-test", req.VMID)
	}
	if got, want := req.MemoryBalloonTargetMiB, int64(8192); got != want {
		t.Fatalf("memory balloon target = %d, want %d", got, want)
	}
}

func TestExecuteInSandboxSkipsMemoryBalloonGrowWhenLaunchAlreadyRestoredTarget(t *testing.T) {
	socketDir, err := os.MkdirTemp("", "cr-balloon-ready-")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(socketDir)
	socketPath := filepath.Join(socketDir, "proxy.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()

		var req vsockexec.ExecRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			serverErr <- err
			return
		}
		if got, want := req.Command, []string{"true"}; len(got) != len(want) || got[0] != want[0] {
			t.Errorf("unexpected guest command: got %#v want %#v", got, want)
		}
		if err := vsockexec.EncodeStreamFrame(conn, vsockexec.ExecStreamFrame{Type: "exit", ExitCode: 0}); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	var helperReqs []helperControlRequest
	adapter := &Adapter{
		helperRequestFn: func(_ context.Context, _ *helperSession, req helperControlRequest) (helperControlResponse, error) {
			helperReqs = append(helperReqs, req)
			return helperControlResponse{OK: true}, nil
		},
	}

	_, err = adapter.executeInSandbox(context.Background(), context.Background(), &sandboxInstance{
		SandboxID: "cr-test",
		VMID:      "vm-test",
		FirecrackerConfig: backend.FirecrackerConfig{
			MemoryMiB: 8192,
		},
		ProxySocketPath:        socketPath,
		Policy:                 &policy.CompiledPolicy{NetworkDefault: "deny"},
		Helper:                 &helperSession{},
		exitedCh:               make(chan struct{}),
		memoryBalloonTargetMiB: 8192,
	}, backend.ExecutionRequest{
		SandboxID:   "cr-test",
		ExecutionID: "run-123",
		Command:     []string{"true"},
		Policy:      &policy.CompiledPolicy{NetworkDefault: "deny"},
	}, backend.OutputStream{})
	if err != nil {
		t.Fatalf("executeInSandbox returned error: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("guest exec server returned error: %v", err)
	}
	if len(helperReqs) != 0 {
		t.Fatalf("helper requests = %#v, want no SetMemoryBalloonTarget request when launch already restored target", helperReqs)
	}
}

func TestSandboxInstanceGrowsMemoryBalloonTargetOnce(t *testing.T) {
	var helperReqs []helperControlRequest
	adapter := &Adapter{
		helperRequestFn: func(_ context.Context, _ *helperSession, req helperControlRequest) (helperControlResponse, error) {
			helperReqs = append(helperReqs, req)
			return helperControlResponse{OK: true}, nil
		},
	}
	instance := &sandboxInstance{
		VMID:   "vm-test",
		Helper: &helperSession{},
		FirecrackerConfig: backend.FirecrackerConfig{
			MemoryMiB: 8192,
		},
		memoryBalloonTargetMiB: 1024,
	}

	if err := instance.growDarwinVZMemoryBalloonTarget(context.Background(), adapter); err != nil {
		t.Fatalf("first grow returned error: %v", err)
	}
	if err := instance.growDarwinVZMemoryBalloonTarget(context.Background(), adapter); err != nil {
		t.Fatalf("second grow returned error: %v", err)
	}
	if len(helperReqs) != 1 {
		t.Fatalf("helper requests = %#v, want exactly one grow request", helperReqs)
	}
	if got, want := instance.memoryBalloonTargetMiB, int64(8192); got != want {
		t.Fatalf("tracked memory balloon target = %d, want %d", got, want)
	}
}
