package firecracker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/volumestore"
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestCreateSnapshotSyncsPausesAndClonesRootFS(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	rootfsPath := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfsPath, []byte("snapshot-bytes"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}

	var signals []syscall.Signal
	prevSignal := sendProcessSignal
	sendProcessSignal = func(_ *os.Process, sig syscall.Signal) error {
		signals = append(signals, sig)
		return nil
	}
	t.Cleanup(func() { sendProcessSignal = prevSignal })

	adapter := &Adapter{
		runGuestCommandFn: func(_ context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, req vsockexec.ExecRequest, _ backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
			if len(req.Command) != 1 || req.Command[0] != "sync" {
				t.Fatalf("unexpected command: %v", req.Command)
			}
			return vsockexec.ExecResponse{ExitCode: 0}, guestExecTiming{}, nil
		},
		sandboxes: map[string]*sandboxInstance{
			"cr-test": {
				SandboxID:    "cr-test",
				VsockPath:    "/tmp/fake.sock",
				GuestPort:    10700,
				fcCmd:        &exec.Cmd{Process: &os.Process{Pid: 42}},
				exitedCh:     make(chan struct{}),
				vmRootFSPath: rootfsPath,
			},
		},
	}

	result, err := adapter.CreateSnapshot(context.Background(), backend.SnapshotRequest{
		SandboxID:  "cr-test",
		SnapshotID: "snap-test",
		FirecrackerConfig: backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}
	if got, want := result.StorageRef, filepath.Join(stateHome, "cleanroom", "snapshots", "firecracker", "snap-test", "rootfs.ext4"); got != want {
		t.Fatalf("unexpected snapshot storage ref: got %q want %q", got, want)
	}

	data, err := os.ReadFile(result.StorageRef)
	if err != nil {
		t.Fatalf("read snapshot rootfs: %v", err)
	}
	if got, want := string(data), "snapshot-bytes"; got != want {
		t.Fatalf("unexpected snapshot contents: got %q want %q", got, want)
	}
	if len(signals) != 2 || signals[0] != syscall.SIGSTOP || signals[1] != syscall.SIGCONT {
		t.Fatalf("unexpected signals: %v", signals)
	}
}

func TestCreateSnapshotReturnsErrorWhenGuestSyncExitsNonZero(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	rootfsPath := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfsPath, []byte("snapshot-bytes"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}

	prevSignal := sendProcessSignal
	sendProcessSignal = func(_ *os.Process, _ syscall.Signal) error { return nil }
	t.Cleanup(func() { sendProcessSignal = prevSignal })

	adapter := &Adapter{
		runGuestCommandFn: func(_ context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, req vsockexec.ExecRequest, _ backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
			if len(req.Command) != 1 || req.Command[0] != "sync" {
				t.Fatalf("unexpected command: %v", req.Command)
			}
			return vsockexec.ExecResponse{ExitCode: 42, Error: "sync failed"}, guestExecTiming{}, nil
		},
		sandboxes: map[string]*sandboxInstance{
			"cr-test": {
				SandboxID:    "cr-test",
				VsockPath:    "/tmp/fake.sock",
				GuestPort:    10700,
				fcCmd:        &exec.Cmd{Process: &os.Process{Pid: 42}},
				exitedCh:     make(chan struct{}),
				vmRootFSPath: rootfsPath,
			},
		},
	}

	_, err := adapter.CreateSnapshot(context.Background(), backend.SnapshotRequest{
		SandboxID:  "cr-test",
		SnapshotID: "snap-test",
		FirecrackerConfig: backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{Enabled: true},
		},
	})
	if err == nil {
		t.Fatal("expected CreateSnapshot to fail when guest sync exits non-zero")
	}
}

func TestCreateSnapshotUsesConfiguredSnapshotBaseDir(t *testing.T) {
	rootfsPath := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfsPath, []byte("snapshot-bytes"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}

	prevSignal := sendProcessSignal
	sendProcessSignal = func(_ *os.Process, _ syscall.Signal) error { return nil }
	t.Cleanup(func() { sendProcessSignal = prevSignal })

	adapter := &Adapter{
		runGuestCommandFn: func(_ context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, req vsockexec.ExecRequest, _ backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
			if len(req.Command) != 1 || req.Command[0] != "sync" {
				t.Fatalf("unexpected command: %v", req.Command)
			}
			return vsockexec.ExecResponse{ExitCode: 0}, guestExecTiming{}, nil
		},
		sandboxes: map[string]*sandboxInstance{
			"cr-test": {
				SandboxID:    "cr-test",
				VsockPath:    "/tmp/fake.sock",
				GuestPort:    10700,
				fcCmd:        &exec.Cmd{Process: &os.Process{Pid: 42}},
				exitedCh:     make(chan struct{}),
				vmRootFSPath: rootfsPath,
			},
		},
	}

	baseDir := filepath.Join(t.TempDir(), "configured-snapshots")
	result, err := adapter.CreateSnapshot(context.Background(), backend.SnapshotRequest{
		SandboxID:  "cr-test",
		SnapshotID: "snap-test",
		FirecrackerConfig: backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{Enabled: true, BaseDir: baseDir},
		},
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}
	if got, want := result.StorageRef, filepath.Join(baseDir, "firecracker", "snap-test", "rootfs.ext4"); got != want {
		t.Fatalf("unexpected snapshot storage ref: got %q want %q", got, want)
	}
}

func TestCreateSnapshotReturnsErrorWhenSandboxResumeFails(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	rootfsPath := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfsPath, []byte("snapshot-bytes"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}

	prevSignal := sendProcessSignal
	sendProcessSignal = func(_ *os.Process, sig syscall.Signal) error {
		if sig == syscall.SIGCONT {
			return os.ErrPermission
		}
		return nil
	}
	t.Cleanup(func() { sendProcessSignal = prevSignal })

	adapter := &Adapter{
		runGuestCommandFn: func(_ context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, req vsockexec.ExecRequest, _ backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
			if len(req.Command) != 1 || req.Command[0] != "sync" {
				t.Fatalf("unexpected command: %v", req.Command)
			}
			return vsockexec.ExecResponse{ExitCode: 0}, guestExecTiming{}, nil
		},
		sandboxes: map[string]*sandboxInstance{
			"cr-test": {
				SandboxID:    "cr-test",
				VsockPath:    "/tmp/fake.sock",
				GuestPort:    10700,
				fcCmd:        &exec.Cmd{Process: &os.Process{Pid: 42}},
				exitedCh:     make(chan struct{}),
				vmRootFSPath: rootfsPath,
			},
		},
	}

	result, err := adapter.CreateSnapshot(context.Background(), backend.SnapshotRequest{
		SandboxID:  "cr-test",
		SnapshotID: "snap-test",
		FirecrackerConfig: backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{Enabled: true},
		},
	})
	if err == nil {
		t.Fatal("expected CreateSnapshot to fail when sandbox resume fails")
	}
	if result != nil {
		t.Fatalf("expected nil snapshot result, got %#v", result)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "resume") {
		t.Fatalf("expected resume error, got %v", err)
	}
	snapshotPath := filepath.Join(stateHome, "cleanroom", "snapshots", "firecracker", "snap-test", "rootfs.ext4")
	if _, statErr := os.Stat(snapshotPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected failed snapshot to be cleaned up, stat error = %v", statErr)
	}
}

func TestProvisionSandboxFromSnapshotUsesSnapshotRootFS(t *testing.T) {
	t.Parallel()

	var (
		gotRootFS  string
		gotSandbox string
		gotPolicy  *policy.CompiledPolicy
	)
	adapter := &Adapter{
		launchSandboxVMFromRootFSFn: func(_ context.Context, sandboxID string, compiled *policy.CompiledPolicy, _ backend.FirecrackerConfig, sourceRootFSPath string) (*sandboxInstance, error) {
			gotSandbox = sandboxID
			gotPolicy = compiled
			gotRootFS = sourceRootFSPath
			return &sandboxInstance{SandboxID: sandboxID}, nil
		},
	}
	compiled := &policy.CompiledPolicy{
		NetworkDefault: "deny",
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Hash:           "policy-hash",
	}

	if err := adapter.ProvisionSandboxFromSnapshot(context.Background(), backend.ProvisionFromSnapshotRequest{
		SandboxID:  "cr-test",
		SnapshotID: "snap-test",
		StorageRef: "/tmp/snap-test.ext4",
		Policy:     compiled,
	}); err != nil {
		t.Fatalf("ProvisionSandboxFromSnapshot returned error: %v", err)
	}
	if got, want := gotSandbox, "cr-test"; got != want {
		t.Fatalf("unexpected sandbox id: got %q want %q", got, want)
	}
	if got, want := gotRootFS, "/tmp/snap-test.ext4"; got != want {
		t.Fatalf("unexpected source rootfs: got %q want %q", got, want)
	}
	if gotPolicy != compiled {
		t.Fatal("expected compiled policy to be forwarded")
	}
	if _, ok := adapter.sandboxes["cr-test"]; !ok {
		t.Fatal("expected provisioned sandbox to be stored")
	}
}

func TestRootFSVolumeStoreDriverAllowsWritableVolumesWhenSnapshotsDisabled(t *testing.T) {
	t.Parallel()

	sourcePath := filepath.Join(t.TempDir(), "base.ext4")
	if err := os.WriteFile(sourcePath, []byte("rootfs-bytes"), 0o644); err != nil {
		t.Fatalf("write base volume: %v", err)
	}

	driver, err := rootFSVolumeStoreDriver(backend.FirecrackerConfig{})
	if err != nil {
		t.Fatalf("rootFSVolumeStoreDriver returned error: %v", err)
	}

	base, err := driver.EnsureBaseVolume(context.Background(), volumestore.EnsureBaseVolumeRequest{
		BaseID:     "base",
		SourcePath: sourcePath,
	})
	if err != nil {
		t.Fatalf("EnsureBaseVolume returned error: %v", err)
	}

	attachmentPath := filepath.Join(t.TempDir(), "writable.ext4")
	volume, err := driver.CreateWritableVolume(context.Background(), volumestore.CreateWritableVolumeRequest{
		VolumeID:       "sandbox",
		BaseRef:        base.Ref,
		AttachmentPath: attachmentPath,
	})
	if err != nil {
		t.Fatalf("CreateWritableVolume returned error: %v", err)
	}
	if got, want := volume.AttachmentPath, attachmentPath; got != want {
		t.Fatalf("unexpected attachment path: got %q want %q", got, want)
	}

	data, err := os.ReadFile(attachmentPath)
	if err != nil {
		t.Fatalf("read writable volume: %v", err)
	}
	if got, want := string(data), "rootfs-bytes"; got != want {
		t.Fatalf("unexpected writable volume contents: got %q want %q", got, want)
	}
}

func TestSnapshotVolumeStoreDriverRejectsDisabledSnapshots(t *testing.T) {
	t.Parallel()

	_, err := snapshotVolumeStoreDriver(backend.FirecrackerConfig{})
	if err == nil {
		t.Fatal("expected snapshotVolumeStoreDriver to reject disabled snapshots")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "not enabled") {
		t.Fatalf("unexpected error: %v", err)
	}
}
