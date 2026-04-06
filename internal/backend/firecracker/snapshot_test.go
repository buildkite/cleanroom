package firecracker

import (
	"bytes"
	"context"
	"errors"
	"log"
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

type testVolumeDriver struct {
	ensureBaseVolumeFn      func(context.Context, volumestore.EnsureBaseVolumeRequest) (volumestore.BaseVolume, error)
	createWritableVolumeFn  func(context.Context, volumestore.CreateWritableVolumeRequest) (volumestore.WritableVolume, error)
	snapshotVolumeFn        func(context.Context, volumestore.SnapshotVolumeRequest) (volumestore.Snapshot, error)
	cloneSnapshotToVolumeFn func(context.Context, volumestore.CloneSnapshotToVolumeRequest) (volumestore.WritableVolume, error)
	destroyVolumeFn         func(context.Context, volumestore.DestroyVolumeRequest) error
	destroySnapshotFn       func(context.Context, volumestore.DestroySnapshotRequest) error
}

func (d testVolumeDriver) Name() string { return "test" }

func (d testVolumeDriver) EnsureBaseVolume(ctx context.Context, req volumestore.EnsureBaseVolumeRequest) (volumestore.BaseVolume, error) {
	if d.ensureBaseVolumeFn == nil {
		return volumestore.BaseVolume{}, errors.New("unexpected EnsureBaseVolume call")
	}
	return d.ensureBaseVolumeFn(ctx, req)
}

func (d testVolumeDriver) CreateWritableVolume(ctx context.Context, req volumestore.CreateWritableVolumeRequest) (volumestore.WritableVolume, error) {
	if d.createWritableVolumeFn == nil {
		return volumestore.WritableVolume{}, errors.New("unexpected CreateWritableVolume call")
	}
	return d.createWritableVolumeFn(ctx, req)
}

func (d testVolumeDriver) SnapshotVolume(ctx context.Context, req volumestore.SnapshotVolumeRequest) (volumestore.Snapshot, error) {
	if d.snapshotVolumeFn == nil {
		return volumestore.Snapshot{}, errors.New("unexpected SnapshotVolume call")
	}
	return d.snapshotVolumeFn(ctx, req)
}

func (d testVolumeDriver) CloneSnapshotToVolume(ctx context.Context, req volumestore.CloneSnapshotToVolumeRequest) (volumestore.WritableVolume, error) {
	if d.cloneSnapshotToVolumeFn == nil {
		return volumestore.WritableVolume{}, errors.New("unexpected CloneSnapshotToVolume call")
	}
	return d.cloneSnapshotToVolumeFn(ctx, req)
}

func (d testVolumeDriver) DestroyVolume(ctx context.Context, req volumestore.DestroyVolumeRequest) error {
	if d.destroyVolumeFn == nil {
		return nil
	}
	return d.destroyVolumeFn(ctx, req)
}

func (d testVolumeDriver) DestroySnapshot(ctx context.Context, req volumestore.DestroySnapshotRequest) error {
	if d.destroySnapshotFn == nil {
		return nil
	}
	return d.destroySnapshotFn(ctx, req)
}

func captureFirecrackerLogOutput(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	prevPrefix := log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})
	return &buf
}

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

func TestCreateSnapshotFlushesHostFilesystemForZFSSnapshots(t *testing.T) {
	prevSignal := sendProcessSignal
	sendProcessSignal = func(_ *os.Process, _ syscall.Signal) error { return nil }
	t.Cleanup(func() { sendProcessSignal = prevSignal })

	prevSync := syncHostFilesystem
	prevSnapshotDriver := snapshotVolumeStoreDriverFn
	t.Cleanup(func() {
		syncHostFilesystem = prevSync
		snapshotVolumeStoreDriverFn = prevSnapshotDriver
	})

	var calls []string
	syncHostFilesystem = func(context.Context) error {
		calls = append(calls, "host-sync")
		return nil
	}
	snapshotVolumeStoreDriverFn = func(cfg backend.FirecrackerConfig) (volumestore.Driver, error) {
		if got, want := cfg.Snapshots.Driver, "zfs"; got != want {
			t.Fatalf("unexpected snapshot driver: got %q want %q", got, want)
		}
		return testVolumeDriver{
			snapshotVolumeFn: func(_ context.Context, req volumestore.SnapshotVolumeRequest) (volumestore.Snapshot, error) {
				if got, want := calls, []string{"host-sync"}; strings.Join(got, ",") != strings.Join(want, ",") {
					t.Fatalf("expected host sync before snapshot, got %v", got)
				}
				if got, want := req.VolumeRef, "tank/cleanroom/sandboxes/cr-test"; got != want {
					t.Fatalf("unexpected volume ref: got %q want %q", got, want)
				}
				calls = append(calls, "snapshot")
				return volumestore.Snapshot{StorageRef: "tank/cleanroom/snapshots/snap-test@seed"}, nil
			},
		}, nil
	}

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
				vmRootFSPath: "/dev/zvol/tank/cleanroom/sandboxes/cr-test",
				volumeRef:    "tank/cleanroom/sandboxes/cr-test",
			},
		},
	}

	result, err := adapter.CreateSnapshot(context.Background(), backend.SnapshotRequest{
		SandboxID:  "cr-test",
		SnapshotID: "snap-test",
		FirecrackerConfig: backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{
				Enabled:    true,
				Driver:     "zfs",
				ZFSDataset: "tank/cleanroom",
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}
	if got, want := result.StorageRef, "tank/cleanroom/snapshots/snap-test@seed"; got != want {
		t.Fatalf("unexpected snapshot storage ref: got %q want %q", got, want)
	}
	if got, want := calls, []string{"host-sync", "snapshot"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected call order: got %v want %v", got, want)
	}
}

func TestCreateSnapshotResumesSandboxWhenHostSyncFails(t *testing.T) {
	prevSync := syncHostFilesystem
	prevSnapshotDriver := snapshotVolumeStoreDriverFn
	prevSignal := sendProcessSignal
	t.Cleanup(func() {
		syncHostFilesystem = prevSync
		snapshotVolumeStoreDriverFn = prevSnapshotDriver
		sendProcessSignal = prevSignal
	})

	var signals []syscall.Signal
	sendProcessSignal = func(_ *os.Process, sig syscall.Signal) error {
		signals = append(signals, sig)
		return nil
	}

	syncHostFilesystem = func(context.Context) error {
		return errors.New("host sync failed")
	}
	snapshotVolumeStoreDriverFn = func(cfg backend.FirecrackerConfig) (volumestore.Driver, error) {
		if got, want := cfg.Snapshots.Driver, "zfs"; got != want {
			t.Fatalf("unexpected snapshot driver: got %q want %q", got, want)
		}
		return testVolumeDriver{
			snapshotVolumeFn: func(context.Context, volumestore.SnapshotVolumeRequest) (volumestore.Snapshot, error) {
				t.Fatal("snapshot should not run after host sync failure")
				return volumestore.Snapshot{}, nil
			},
		}, nil
	}

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
				vmRootFSPath: "/dev/zvol/tank/cleanroom/sandboxes/cr-test",
				volumeRef:    "tank/cleanroom/sandboxes/cr-test",
			},
		},
	}

	result, err := adapter.CreateSnapshot(context.Background(), backend.SnapshotRequest{
		SandboxID:  "cr-test",
		SnapshotID: "snap-test",
		FirecrackerConfig: backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{
				Enabled:    true,
				Driver:     "zfs",
				ZFSDataset: "tank/cleanroom",
			},
		},
	})
	if err == nil {
		t.Fatal("expected CreateSnapshot to fail when host sync fails")
	}
	if result != nil {
		t.Fatalf("expected nil snapshot result, got %#v", result)
	}
	if !strings.Contains(err.Error(), "host sync failed") {
		t.Fatalf("expected host sync error, got %v", err)
	}
	if got, want := signals, []syscall.Signal{syscall.SIGSTOP, syscall.SIGCONT}; strings.Join(signalStrings(got), ",") != strings.Join(signalStrings(want), ",") {
		t.Fatalf("unexpected signals: got %v want %v", got, want)
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

func TestSnapshotDriverNeedsHostSync(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		driverName string
		want       bool
	}{
		{name: "zfs", driverName: "zfs", want: true},
		{name: "trimmed zfs", driverName: " ZFS ", want: true},
		{name: "file", driverName: "file", want: false},
		{name: "empty", driverName: "", want: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := snapshotDriverNeedsHostSync(tc.driverName); got != tc.want {
				t.Fatalf("snapshotDriverNeedsHostSync(%q) = %t, want %t", tc.driverName, got, tc.want)
			}
		})
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

func TestCreateSnapshotUsesManagedVolumeRef(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	managedVolumePath := filepath.Join(t.TempDir(), "managed-volume.ext4")
	if err := os.WriteFile(managedVolumePath, []byte("snapshot-bytes"), 0o644); err != nil {
		t.Fatalf("write managed volume: %v", err)
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
				vmRootFSPath: "/dev/zvol/tank/cleanroom/sandboxes/cr-test",
				volumeRef:    managedVolumePath,
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

	data, err := os.ReadFile(result.StorageRef)
	if err != nil {
		t.Fatalf("read snapshot rootfs: %v", err)
	}
	if got, want := string(data), "snapshot-bytes"; got != want {
		t.Fatalf("unexpected snapshot contents: got %q want %q", got, want)
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

func signalStrings(signals []syscall.Signal) []string {
	out := make([]string, 0, len(signals))
	for _, sig := range signals {
		out = append(out, sig.String())
	}
	return out
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

func TestSnapshotConfigForStorageRefUsesStoredZFSDataset(t *testing.T) {
	t.Parallel()

	cfg, err := snapshotConfigForStorageRef(backend.FirecrackerConfig{
		Snapshots: backend.SnapshotConfig{
			Driver:     "zfs",
			ZFSDataset: "tank/other",
		},
	}, "tank/cleanroom/snapshots/snap-test@seed")
	if err != nil {
		t.Fatalf("snapshotConfigForStorageRef returned error: %v", err)
	}
	if got, want := cfg.Snapshots.ZFSDataset, "tank/cleanroom"; got != want {
		t.Fatalf("unexpected zfs dataset root: got %q want %q", got, want)
	}
}

func TestSnapshotConfigForStorageRefLeavesConfiguredDatasetForNonStoredRef(t *testing.T) {
	t.Parallel()

	cfg, err := snapshotConfigForStorageRef(backend.FirecrackerConfig{
		Snapshots: backend.SnapshotConfig{
			Driver:     "zfs",
			ZFSDataset: "tank/cleanroom",
		},
	}, "tank/cleanroom/sandboxes/sandbox-1@snap-golden")
	if err != nil {
		t.Fatalf("snapshotConfigForStorageRef returned error: %v", err)
	}
	if got, want := cfg.Snapshots.ZFSDataset, "tank/cleanroom"; got != want {
		t.Fatalf("unexpected zfs dataset root: got %q want %q", got, want)
	}
}

func TestSnapshotConfigForStorageRefInfersZFSDriverFromStoredRef(t *testing.T) {
	t.Parallel()

	cfg, err := snapshotConfigForStorageRef(backend.FirecrackerConfig{}, "tank/cleanroom/snapshots/snap-test@seed")
	if err != nil {
		t.Fatalf("snapshotConfigForStorageRef returned error: %v", err)
	}
	if got, want := cfg.Snapshots.Driver, "zfs"; got != want {
		t.Fatalf("unexpected snapshot driver: got %q want %q", got, want)
	}
	if got, want := cfg.Snapshots.ZFSDataset, "tank/cleanroom"; got != want {
		t.Fatalf("unexpected zfs dataset root: got %q want %q", got, want)
	}
}

func TestSnapshotConfigForStorageRefInfersZFSDriverFromManagedVolumeRef(t *testing.T) {
	t.Parallel()

	cfg, err := snapshotConfigForStorageRef(backend.FirecrackerConfig{}, "tank/cleanroom/sandboxes/sandbox-1")
	if err != nil {
		t.Fatalf("snapshotConfigForStorageRef returned error: %v", err)
	}
	if got, want := cfg.Snapshots.Driver, "zfs"; got != want {
		t.Fatalf("unexpected snapshot driver: got %q want %q", got, want)
	}
	if got, want := cfg.Snapshots.ZFSDataset, "tank/cleanroom"; got != want {
		t.Fatalf("unexpected zfs dataset root: got %q want %q", got, want)
	}
}

func TestSnapshotConfigForStorageRefRejectsDriverMismatch(t *testing.T) {
	t.Parallel()

	_, err := snapshotConfigForStorageRef(backend.FirecrackerConfig{
		Snapshots: backend.SnapshotConfig{Driver: "file"},
	}, "tank/cleanroom/snapshots/snap-test@seed")
	if err == nil {
		t.Fatal("expected snapshotConfigForStorageRef to reject driver mismatch")
	}
	if !strings.Contains(err.Error(), "requires zfs driver") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSnapshotConfigForStorageRefIgnoresFilesystemPathsWithZFSNamespaceComponents(t *testing.T) {
	t.Parallel()

	refs := []string{
		"/var/lib/buildkite-agent/state/cleanroom/snapshots/firecracker/snap-test/rootfs.ext4",
		"/var/lib/buildkite-agent/state/cleanroom/sandboxes/cr-test/rootfs-persistent.ext4",
	}
	for _, ref := range refs {
		cfg, err := snapshotConfigForStorageRef(backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{Driver: "file"},
		}, ref)
		if err != nil {
			t.Fatalf("snapshotConfigForStorageRef returned error for %q: %v", ref, err)
		}
		if got, want := cfg.Snapshots.Driver, "file"; got != want {
			t.Fatalf("unexpected snapshot driver for %q: got %q want %q", ref, got, want)
		}
		if cfg.Snapshots.ZFSDataset != "" {
			t.Fatalf("expected no inferred zfs dataset root for %q, got %q", ref, cfg.Snapshots.ZFSDataset)
		}
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

func TestPreparePersistentWritableVolumeUsesBaseVolumeForRootFSPath(t *testing.T) {
	t.Parallel()

	rootfsPath := filepath.Join(t.TempDir(), "runtime-rootfs.ext4")
	if err := os.WriteFile(rootfsPath, []byte("runtime"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}

	var (
		gotEnsureReq volumestore.EnsureBaseVolumeRequest
		gotCreateReq volumestore.CreateWritableVolumeRequest
	)
	driver := testVolumeDriver{
		ensureBaseVolumeFn: func(_ context.Context, req volumestore.EnsureBaseVolumeRequest) (volumestore.BaseVolume, error) {
			gotEnsureReq = req
			return volumestore.BaseVolume{Ref: "base-ref"}, nil
		},
		createWritableVolumeFn: func(_ context.Context, req volumestore.CreateWritableVolumeRequest) (volumestore.WritableVolume, error) {
			gotCreateReq = req
			if err := os.WriteFile(req.AttachmentPath, nil, 0o644); err != nil {
				return volumestore.WritableVolume{}, err
			}
			if err := os.Truncate(req.AttachmentPath, 8<<20); err != nil {
				return volumestore.WritableVolume{}, err
			}
			return volumestore.WritableVolume{Ref: "volume-ref", AttachmentPath: req.AttachmentPath}, nil
		},
	}

	writable, cleanupVolume, err := preparePersistentWritableVolume(context.Background(), driver, "sandbox-1", t.TempDir(), rootfsPath, 8<<20)
	if err != nil {
		t.Fatalf("preparePersistentWritableVolume returned error: %v", err)
	}
	if cleanupVolume == nil {
		t.Fatal("expected cleanup function")
	}
	if got, want := gotEnsureReq.SourcePath, rootfsPath; got != want {
		t.Fatalf("unexpected base source path: got %q want %q", got, want)
	}
	if got, want := gotEnsureReq.BaseID, "runtime-rootfs"; got != want {
		t.Fatalf("unexpected base id: got %q want %q", got, want)
	}
	if got, want := gotEnsureReq.MinimumBytes, int64(8<<20); got != want {
		t.Fatalf("unexpected minimum bytes: got %d want %d", got, want)
	}
	if got, want := gotCreateReq.BaseRef, "base-ref"; got != want {
		t.Fatalf("unexpected base ref: got %q want %q", got, want)
	}
	if got, want := gotCreateReq.VolumeID, "sandbox-1"; got != want {
		t.Fatalf("unexpected volume id: got %q want %q", got, want)
	}
	if got, want := writable.Ref, "volume-ref"; got != want {
		t.Fatalf("unexpected writable volume ref: got %q want %q", got, want)
	}
}

func TestPreparePersistentWritableVolumeUsesSnapshotCloneForSnapshotRef(t *testing.T) {
	t.Parallel()

	var gotCloneReq volumestore.CloneSnapshotToVolumeRequest
	driver := testVolumeDriver{
		cloneSnapshotToVolumeFn: func(_ context.Context, req volumestore.CloneSnapshotToVolumeRequest) (volumestore.WritableVolume, error) {
			gotCloneReq = req
			return volumestore.WritableVolume{Ref: "tank/cleanroom/sandboxes/sandbox-1", AttachmentPath: "/dev/zvol/tank/cleanroom/sandboxes/sandbox-1"}, nil
		},
	}

	writable, cleanupVolume, err := preparePersistentWritableVolume(context.Background(), driver, "sandbox-1", t.TempDir(), "tank/cleanroom/sandboxes/source@snap-golden", 0)
	if err != nil {
		t.Fatalf("preparePersistentWritableVolume returned error: %v", err)
	}
	if cleanupVolume == nil {
		t.Fatal("expected cleanup function")
	}
	if got, want := gotCloneReq.SnapshotRef, "tank/cleanroom/sandboxes/source@snap-golden"; got != want {
		t.Fatalf("unexpected snapshot ref: got %q want %q", got, want)
	}
	if got, want := gotCloneReq.VolumeID, "sandbox-1"; got != want {
		t.Fatalf("unexpected volume id: got %q want %q", got, want)
	}
	if got, want := writable.AttachmentPath, "/dev/zvol/tank/cleanroom/sandboxes/sandbox-1"; got != want {
		t.Fatalf("unexpected attachment path: got %q want %q", got, want)
	}
}

func TestPreparePersistentWritableVolumeCleansUpOnResizeFailure(t *testing.T) {
	t.Parallel()

	var destroyed string
	driver := testVolumeDriver{
		cloneSnapshotToVolumeFn: func(_ context.Context, req volumestore.CloneSnapshotToVolumeRequest) (volumestore.WritableVolume, error) {
			return volumestore.WritableVolume{
				Ref:            "tank/cleanroom/sandboxes/sandbox-1",
				AttachmentPath: filepath.Join(t.TempDir(), "missing.ext4"),
			}, nil
		},
		destroyVolumeFn: func(_ context.Context, req volumestore.DestroyVolumeRequest) error {
			destroyed = req.VolumeRef
			return nil
		},
	}

	_, cleanupVolume, err := preparePersistentWritableVolume(context.Background(), driver, "sandbox-1", t.TempDir(), "tank/cleanroom/sandboxes/source@snap-golden", 8<<20)
	if err == nil {
		t.Fatal("expected preparePersistentWritableVolume to fail")
	}
	if cleanupVolume != nil {
		t.Fatal("expected cleanup function to be discarded on failure")
	}
	if got, want := destroyed, "tank/cleanroom/sandboxes/sandbox-1"; got != want {
		t.Fatalf("unexpected destroyed volume ref: got %q want %q", got, want)
	}
}

func TestPreparePersistentWritableVolumeLogsDestroyFailure(t *testing.T) {
	logOutput := captureFirecrackerLogOutput(t)

	driver := testVolumeDriver{
		cloneSnapshotToVolumeFn: func(_ context.Context, req volumestore.CloneSnapshotToVolumeRequest) (volumestore.WritableVolume, error) {
			return volumestore.WritableVolume{
				Ref:            "tank/cleanroom/sandboxes/sandbox-1",
				AttachmentPath: filepath.Join(t.TempDir(), "writable.ext4"),
			}, nil
		},
		destroyVolumeFn: func(_ context.Context, req volumestore.DestroyVolumeRequest) error {
			return errors.New("destroy failed")
		},
	}

	_, cleanupVolume, err := preparePersistentWritableVolume(context.Background(), driver, "sandbox-1", t.TempDir(), "tank/cleanroom/sandboxes/source@snap-golden", 0)
	if err != nil {
		t.Fatalf("preparePersistentWritableVolume returned error: %v", err)
	}
	if cleanupVolume == nil {
		t.Fatal("expected cleanup function")
	}

	cleanupVolume()

	if got := logOutput.String(); !strings.Contains(got, "firecracker: cleanup persistent volume \"tank/cleanroom/sandboxes/sandbox-1\": destroy failed") {
		t.Fatalf("expected destroy error to be logged, got %q", got)
	}
}

func TestPrepareWritableRootVolumeUsesRootFSVolumeStoreDriver(t *testing.T) {
	t.Parallel()

	rootfsPath := filepath.Join(t.TempDir(), "runtime-rootfs.ext4")
	if err := os.WriteFile(rootfsPath, []byte("runtime"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}

	prevDriverFn := rootFSVolumeStoreDriverFn
	t.Cleanup(func() { rootFSVolumeStoreDriverFn = prevDriverFn })

	var gotDriverCfg backend.FirecrackerConfig
	rootFSVolumeStoreDriverFn = func(cfg backend.FirecrackerConfig) (volumestore.Driver, error) {
		gotDriverCfg = cfg
		return testVolumeDriver{
			ensureBaseVolumeFn: func(_ context.Context, req volumestore.EnsureBaseVolumeRequest) (volumestore.BaseVolume, error) {
				if got, want := req.SourcePath, rootfsPath; got != want {
					t.Fatalf("unexpected base source path: got %q want %q", got, want)
				}
				if got, want := req.MinimumBytes, int64(8<<20); got != want {
					t.Fatalf("unexpected minimum bytes: got %d want %d", got, want)
				}
				return volumestore.BaseVolume{Ref: "base-ref"}, nil
			},
			createWritableVolumeFn: func(_ context.Context, req volumestore.CreateWritableVolumeRequest) (volumestore.WritableVolume, error) {
				if got, want := req.VolumeID, "exec-1"; got != want {
					t.Fatalf("unexpected volume id: got %q want %q", got, want)
				}
				if err := os.WriteFile(req.AttachmentPath, nil, 0o644); err != nil {
					return volumestore.WritableVolume{}, err
				}
				if err := os.Truncate(req.AttachmentPath, 8<<20); err != nil {
					return volumestore.WritableVolume{}, err
				}
				return volumestore.WritableVolume{Ref: "volume-ref", AttachmentPath: req.AttachmentPath}, nil
			},
		}, nil
	}

	writable, cleanupVolume, err := prepareWritableRootVolume(context.Background(), backend.FirecrackerConfig{
		MinimumRootFSBytes: 8 << 20,
	}, "exec-1", t.TempDir(), rootfsPath)
	if err != nil {
		t.Fatalf("prepareWritableRootVolume returned error: %v", err)
	}
	if cleanupVolume == nil {
		t.Fatal("expected cleanup function")
	}
	if got, want := gotDriverCfg.MinimumRootFSBytes, int64(8<<20); got != want {
		t.Fatalf("unexpected driver config minimum bytes: got %d want %d", got, want)
	}
	if got, want := writable.Ref, "volume-ref"; got != want {
		t.Fatalf("unexpected writable volume ref: got %q want %q", got, want)
	}
}

func TestPrepareWritableRootVolumeNormalizesManagedZFSStorageRefs(t *testing.T) {
	t.Parallel()

	prevDriverFn := rootFSVolumeStoreDriverFn
	t.Cleanup(func() { rootFSVolumeStoreDriverFn = prevDriverFn })

	var gotDriverCfg backend.FirecrackerConfig
	rootFSVolumeStoreDriverFn = func(cfg backend.FirecrackerConfig) (volumestore.Driver, error) {
		gotDriverCfg = cfg
		return testVolumeDriver{
			cloneSnapshotToVolumeFn: func(_ context.Context, req volumestore.CloneSnapshotToVolumeRequest) (volumestore.WritableVolume, error) {
				return volumestore.WritableVolume{Ref: "tank/cleanroom/sandboxes/exec-1", AttachmentPath: req.AttachmentPath}, nil
			},
		}, nil
	}

	_, cleanupVolume, err := prepareWritableRootVolume(
		context.Background(),
		backend.FirecrackerConfig{},
		"exec-1",
		t.TempDir(),
		"tank/cleanroom/sandboxes/source@snap-golden",
	)
	if err != nil {
		t.Fatalf("prepareWritableRootVolume returned error: %v", err)
	}
	if cleanupVolume == nil {
		t.Fatal("expected cleanup function")
	}
	if got, want := gotDriverCfg.Snapshots.Driver, "zfs"; got != want {
		t.Fatalf("unexpected snapshot driver: got %q want %q", got, want)
	}
	if got, want := gotDriverCfg.Snapshots.ZFSDataset, "tank/cleanroom"; got != want {
		t.Fatalf("unexpected zfs dataset: got %q want %q", got, want)
	}
}
