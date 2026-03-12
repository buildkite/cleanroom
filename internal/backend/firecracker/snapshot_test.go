package firecracker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/volumestore"
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

	var quiesceCalled bool
	adapter := &Adapter{
		quiesceGuestFn: func(_ context.Context, _ *sandboxInstance, _ int64) error {
			quiesceCalled = true
			return nil
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
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}
	if !quiesceCalled {
		t.Fatal("expected quiesceGuestFn to be called")
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

func TestCreateSnapshotUsesConfiguredSnapshotBaseDir(t *testing.T) {
	rootfsPath := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfsPath, []byte("snapshot-bytes"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}

	prevSignal := sendProcessSignal
	sendProcessSignal = func(_ *os.Process, _ syscall.Signal) error { return nil }
	t.Cleanup(func() { sendProcessSignal = prevSignal })

	adapter := &Adapter{
		quiesceGuestFn: func(_ context.Context, _ *sandboxInstance, _ int64) error {
			return nil
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
			Snapshots: backend.SnapshotConfig{BaseDir: baseDir},
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
		quiesceGuestFn: func(_ context.Context, _ *sandboxInstance, _ int64) error {
			return nil
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

func TestRestoreSandboxReplacesRunningInstance(t *testing.T) {
	t.Parallel()

	compiled := &policy.CompiledPolicy{
		NetworkDefault: "deny",
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Hash:           "policy-hash",
	}
	oldInstance := &sandboxInstance{SandboxID: "cr-test"}
	newInstance := &sandboxInstance{SandboxID: "cr-test", RunDir: "/tmp/new"}
	adapter := &Adapter{
		sandboxes: map[string]*sandboxInstance{"cr-test": oldInstance},
		launchSandboxVMFromRootFSFn: func(_ context.Context, sandboxID string, gotPolicy *policy.CompiledPolicy, _ backend.FirecrackerConfig, sourceRootFSPath string) (*sandboxInstance, error) {
			if sandboxID != "cr-test" {
				t.Fatalf("unexpected sandbox id: %q", sandboxID)
			}
			if gotPolicy != compiled {
				t.Fatal("expected compiled policy to be reused during restore")
			}
			if got, want := sourceRootFSPath, "/tmp/snap-test.ext4"; got != want {
				t.Fatalf("unexpected source rootfs: got %q want %q", got, want)
			}
			return newInstance, nil
		},
	}

	if err := adapter.RestoreSandbox(context.Background(), backend.RestoreRequest{
		SandboxID:  "cr-test",
		SnapshotID: "snap-test",
		StorageRef: "/tmp/snap-test.ext4",
		Policy:     compiled,
	}); err != nil {
		t.Fatalf("RestoreSandbox returned error: %v", err)
	}
	if got, want := adapter.sandboxes["cr-test"], newInstance; got != want {
		t.Fatalf("expected replacement sandbox instance, got %#v want %#v", got, want)
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
			return volumestore.WritableVolume{Ref: "volume-ref", AttachmentPath: req.AttachmentPath}, nil
		},
	}

	writable, err := preparePersistentWritableVolume(context.Background(), driver, "sandbox-1", t.TempDir(), rootfsPath)
	if err != nil {
		t.Fatalf("preparePersistentWritableVolume returned error: %v", err)
	}
	if got, want := gotEnsureReq.SourcePath, rootfsPath; got != want {
		t.Fatalf("unexpected base source path: got %q want %q", got, want)
	}
	if got, want := gotEnsureReq.BaseID, baseVolumeID(rootfsPath); got != want {
		t.Fatalf("unexpected base id: got %q want %q", got, want)
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

func TestBaseVolumeIDDistinguishesDifferentPaths(t *testing.T) {
	t.Parallel()

	idA := baseVolumeID("/cache/firecracker/runtime-rootfs/abc123.ext4")
	idB := baseVolumeID("/cache/firecracker/runtime-rootfs/def456.ext4")
	if idA == idB {
		t.Fatalf("expected different base volume IDs for different paths, got %q", idA)
	}

	// Same path must produce the same ID.
	idA2 := baseVolumeID("/cache/firecracker/runtime-rootfs/abc123.ext4")
	if idA != idA2 {
		t.Fatalf("expected stable base volume ID, got %q and %q", idA, idA2)
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

	writable, err := preparePersistentWritableVolume(context.Background(), driver, "sandbox-1", t.TempDir(), "tank/cleanroom/sandboxes/source@snap-golden")
	if err != nil {
		t.Fatalf("preparePersistentWritableVolume returned error: %v", err)
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
