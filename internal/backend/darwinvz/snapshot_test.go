//go:build darwin

package darwinvz

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/volumestore"
	"golang.org/x/sys/unix"
)

func TestCreateSnapshotSyncsPausesAndClonesRootFS(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	rootfsPath := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfsPath, []byte("snapshot-bytes"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}

	var helperOps []string
	adapter := &Adapter{
		executeInSandboxFn: func(_ context.Context, _ context.Context, instance *sandboxInstance, req backend.RunRequest, _ backend.OutputStream) (*backend.RunResult, error) {
			if instance == nil || instance.SandboxID != "cr-test" {
				t.Fatalf("unexpected sandbox instance: %#v", instance)
			}
			if len(req.Command) != 1 || req.Command[0] != "sync" {
				t.Fatalf("unexpected command: %v", req.Command)
			}
			return &backend.RunResult{ExitCode: 0}, nil
		},
		helperRequestFn: func(_ context.Context, helper *helperSession, req helperControlRequest) (helperControlResponse, error) {
			if helper == nil {
				t.Fatal("expected helper session")
			}
			helperOps = append(helperOps, req.Op+":"+req.VMID)
			return helperControlResponse{OK: true}, nil
		},
		sandboxes: map[string]*sandboxInstance{
			"cr-test": {
				SandboxID:    "cr-test",
				Helper:       &helperSession{},
				VMID:         "vm-test",
				Policy:       &policy.CompiledPolicy{NetworkDefault: "deny"},
				vmRootFSPath: rootfsPath,
				exitedCh:     make(chan struct{}),
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
	if got, want := result.StorageRef, filepath.Join(stateHome, "cleanroom", "snapshots", "darwin-vz", "snap-test", "rootfs.ext4"); got != want {
		t.Fatalf("unexpected snapshot storage ref: got %q want %q", got, want)
	}
	if got, want := strings.Join(helperOps, ","), "PauseVM:vm-test,ResumeVM:vm-test"; got != want {
		t.Fatalf("unexpected helper ops: got %q want %q", got, want)
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
		gotSandbox string
		gotPolicy  *policy.CompiledPolicy
		gotCfg     backend.FirecrackerConfig
	)
	adapter := &Adapter{
		launchSandboxVMFn: func(_ context.Context, sandboxID string, compiled *policy.CompiledPolicy, cfg backend.FirecrackerConfig) (*sandboxInstance, error) {
			gotSandbox = sandboxID
			gotPolicy = compiled
			gotCfg = cfg
			return &sandboxInstance{SandboxID: sandboxID}, nil
		},
	}
	compiled := &policy.CompiledPolicy{
		NetworkDefault: "deny",
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Hash:           "policy-hash",
	}
	snapshotRootFSPath := filepath.Join(t.TempDir(), "snap-test.ext4")
	if err := os.WriteFile(snapshotRootFSPath, []byte("snapshot-rootfs"), 0o644); err != nil {
		t.Fatalf("write snapshot rootfs: %v", err)
	}

	if err := adapter.ProvisionSandboxFromSnapshot(context.Background(), backend.ProvisionFromSnapshotRequest{
		SandboxID:  "cr-test",
		SnapshotID: "snap-test",
		StorageRef: snapshotRootFSPath,
		Policy:     compiled,
	}); err != nil {
		t.Fatalf("ProvisionSandboxFromSnapshot returned error: %v", err)
	}
	if got, want := gotSandbox, "cr-test"; got != want {
		t.Fatalf("unexpected sandbox id: got %q want %q", got, want)
	}
	if gotPolicy != compiled {
		t.Fatal("expected compiled policy to be forwarded")
	}
	if got, want := gotCfg.RootFSPath, snapshotRootFSPath; got != want {
		t.Fatalf("unexpected snapshot rootfs path: got %q want %q", got, want)
	}
	if _, ok := adapter.sandboxes["cr-test"]; !ok {
		t.Fatal("expected provisioned sandbox to be stored")
	}
}

func TestProvisionSandboxFromSnapshotRejectsMissingSnapshotRootFS(t *testing.T) {
	t.Parallel()

	missingSnapshotPath := filepath.Join(t.TempDir(), "missing-rootfs.ext4")
	launchCalled := false
	adapter := &Adapter{
		launchSandboxVMFn: func(_ context.Context, sandboxID string, _ *policy.CompiledPolicy, _ backend.FirecrackerConfig) (*sandboxInstance, error) {
			launchCalled = true
			return &sandboxInstance{SandboxID: sandboxID}, nil
		},
	}

	err := adapter.ProvisionSandboxFromSnapshot(context.Background(), backend.ProvisionFromSnapshotRequest{
		SandboxID:  "cr-test",
		SnapshotID: "snap-test",
		StorageRef: missingSnapshotPath,
		Policy: &policy.CompiledPolicy{
			NetworkDefault: "deny",
			ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	})
	if err == nil {
		t.Fatal("expected ProvisionSandboxFromSnapshot to fail when snapshot rootfs is missing")
	}
	if launchCalled {
		t.Fatal("expected missing snapshot rootfs to fail before launching sandbox")
	}
}

func TestDeleteSnapshotRemovesSnapshotRootFS(t *testing.T) {
	t.Parallel()

	snapshotDir := filepath.Join(t.TempDir(), "snapshots", "darwin-vz", "snap-test")
	snapshotPath := filepath.Join(snapshotDir, "rootfs.ext4")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("mkdir snapshot dir: %v", err)
	}
	if err := os.WriteFile(snapshotPath, []byte("snapshot-bytes"), 0o644); err != nil {
		t.Fatalf("write snapshot rootfs: %v", err)
	}

	adapter := &Adapter{}
	if err := adapter.DeleteSnapshot(context.Background(), backend.DeleteSnapshotRequest{
		SnapshotID: "snap-test",
		StorageRef: snapshotPath,
		FirecrackerConfig: backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{Enabled: true},
		},
	}); err != nil {
		t.Fatalf("DeleteSnapshot returned error: %v", err)
	}
	if _, err := os.Stat(snapshotPath); !os.IsNotExist(err) {
		t.Fatalf("expected snapshot to be removed, got err=%v", err)
	}
}

func TestSnapshotVolumeDriverRejectsDisabledSnapshots(t *testing.T) {
	t.Parallel()

	_, err := snapshotVolumeDriver(backend.FirecrackerConfig{})
	if err == nil {
		t.Fatal("expected snapshotVolumeDriver to reject disabled snapshots")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "not enabled") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRootFSVolumeDriverAllowsAPFSDriver(t *testing.T) {
	t.Parallel()

	driver, err := rootFSVolumeDriver(backend.FirecrackerConfig{
		Snapshots: backend.SnapshotConfig{Driver: "apfs"},
	})
	if err != nil {
		t.Fatalf("rootFSVolumeDriver returned error: %v", err)
	}
	if got, want := driver.Name(), "apfs"; got != want {
		t.Fatalf("unexpected rootfs driver name: got %q want %q", got, want)
	}
}

func TestRootFSVolumeDriverDefaultsToAPFSWhenUnset(t *testing.T) {
	t.Parallel()

	driver, err := rootFSVolumeDriver(backend.FirecrackerConfig{})
	if err != nil {
		t.Fatalf("rootFSVolumeDriver returned error: %v", err)
	}
	if got, want := driver.Name(), "apfs"; got != want {
		t.Fatalf("unexpected default rootfs driver name: got %q want %q", got, want)
	}
}

func TestRootFSVolumeDriverDefaultedAPFSFallsBackToFileSemantics(t *testing.T) {
	apfs := &stubVolumeDriver{
		name:             "apfs",
		baseVolume:       volumestore.BaseVolume{Ref: "base-ref"},
		writableErr:      fmt.Errorf("clonefile create writable volume: %w", unix.EXDEV),
		snapshotErr:      fmt.Errorf("clonefile snapshot volume: %w", unix.ENOTSUP),
		cloneSnapshotErr: fmt.Errorf("clonefile clone snapshot: %w", unix.ENOSYS),
	}
	file := &stubVolumeDriver{
		name:           "file",
		baseVolume:     volumestore.BaseVolume{Ref: "base-ref"},
		writableVolume: volumestore.WritableVolume{Ref: "writable-ref", AttachmentPath: "writable-path"},
		snapshot:       volumestore.Snapshot{Ref: "snapshot-ref", StorageRef: "snapshot-storage"},
		clonedVolume:   volumestore.WritableVolume{Ref: "clone-ref", AttachmentPath: "clone-path"},
	}

	driver, err := newRootFSVolumeDriver(backend.FirecrackerConfig{}, func(string) (volumestore.Driver, error) {
		return file, nil
	}, func(string) (volumestore.Driver, error) {
		return apfs, nil
	})
	if err != nil {
		t.Fatalf("newRootFSVolumeDriver returned error: %v", err)
	}
	if got, want := driver.Name(), "apfs"; got != want {
		t.Fatalf("unexpected default rootfs driver name: got %q want %q", got, want)
	}

	base, err := driver.EnsureBaseVolume(context.Background(), volumestore.EnsureBaseVolumeRequest{
		BaseID:     "runtime-key",
		SourcePath: "/tmp/source.ext4",
	})
	if err != nil {
		t.Fatalf("EnsureBaseVolume returned error: %v", err)
	}
	if got, want := base.Ref, "base-ref"; got != want {
		t.Fatalf("unexpected base ref: got %q want %q", got, want)
	}

	writable, err := driver.CreateWritableVolume(context.Background(), volumestore.CreateWritableVolumeRequest{
		VolumeID:       "sandbox-1",
		BaseRef:        base.Ref,
		AttachmentPath: "/tmp/writable.ext4",
	})
	if err != nil {
		t.Fatalf("CreateWritableVolume returned error: %v", err)
	}
	if got, want := writable.Ref, "writable-ref"; got != want {
		t.Fatalf("unexpected fallback writable ref: got %q want %q", got, want)
	}

	snapshot, err := driver.SnapshotVolume(context.Background(), volumestore.SnapshotVolumeRequest{
		SnapshotID: "snap-1",
		VolumeRef:  writable.Ref,
	})
	if err != nil {
		t.Fatalf("SnapshotVolume returned error: %v", err)
	}
	if got, want := snapshot.Ref, "snapshot-ref"; got != want {
		t.Fatalf("unexpected fallback snapshot ref: got %q want %q", got, want)
	}

	clone, err := driver.CloneSnapshotToVolume(context.Background(), volumestore.CloneSnapshotToVolumeRequest{
		VolumeID:       "sandbox-2",
		SnapshotRef:    snapshot.Ref,
		AttachmentPath: "/tmp/clone.ext4",
	})
	if err != nil {
		t.Fatalf("CloneSnapshotToVolume returned error: %v", err)
	}
	if got, want := clone.Ref, "clone-ref"; got != want {
		t.Fatalf("unexpected fallback clone ref: got %q want %q", got, want)
	}

	if got, want := apfs.ensureBaseCalls, 1; got != want {
		t.Fatalf("unexpected apfs ensure base call count: got %d want %d", got, want)
	}
	if got, want := apfs.writableCalls, 1; got != want {
		t.Fatalf("unexpected apfs writable call count: got %d want %d", got, want)
	}
	if got, want := apfs.snapshotCalls, 1; got != want {
		t.Fatalf("unexpected apfs snapshot call count: got %d want %d", got, want)
	}
	if got, want := apfs.cloneSnapshotCalls, 1; got != want {
		t.Fatalf("unexpected apfs clone call count: got %d want %d", got, want)
	}
	if got, want := file.ensureBaseCalls, 0; got != want {
		t.Fatalf("unexpected file ensure base call count: got %d want %d", got, want)
	}
	if got, want := file.writableCalls, 1; got != want {
		t.Fatalf("unexpected file writable call count: got %d want %d", got, want)
	}
	if got, want := file.snapshotCalls, 1; got != want {
		t.Fatalf("unexpected file snapshot call count: got %d want %d", got, want)
	}
	if got, want := file.cloneSnapshotCalls, 1; got != want {
		t.Fatalf("unexpected file clone call count: got %d want %d", got, want)
	}
}

func TestRootFSVolumeDriverExplicitAPFSDoesNotFallback(t *testing.T) {
	apfsErr := fmt.Errorf("clonefile create writable volume: %w", unix.EXDEV)
	apfs := &stubVolumeDriver{
		name:        "apfs",
		baseVolume:  volumestore.BaseVolume{Ref: "base-ref"},
		writableErr: apfsErr,
	}
	file := &stubVolumeDriver{
		name:           "file",
		baseVolume:     volumestore.BaseVolume{Ref: "base-ref"},
		writableVolume: volumestore.WritableVolume{Ref: "writable-ref", AttachmentPath: "writable-path"},
	}

	driver, err := newRootFSVolumeDriver(backend.FirecrackerConfig{
		Snapshots: backend.SnapshotConfig{Driver: "apfs"},
	}, func(string) (volumestore.Driver, error) {
		return file, nil
	}, func(string) (volumestore.Driver, error) {
		return apfs, nil
	})
	if err != nil {
		t.Fatalf("newRootFSVolumeDriver returned error: %v", err)
	}

	base, err := driver.EnsureBaseVolume(context.Background(), volumestore.EnsureBaseVolumeRequest{
		BaseID:     "runtime-key",
		SourcePath: "/tmp/source.ext4",
	})
	if err != nil {
		t.Fatalf("EnsureBaseVolume returned error: %v", err)
	}

	_, err = driver.CreateWritableVolume(context.Background(), volumestore.CreateWritableVolumeRequest{
		VolumeID:       "sandbox-1",
		BaseRef:        base.Ref,
		AttachmentPath: "/tmp/writable.ext4",
	})
	if err == nil {
		t.Fatal("expected explicit apfs driver to return clonefile error")
	}
	if err.Error() != apfsErr.Error() {
		t.Fatalf("unexpected apfs error: got %v want %v", err, apfsErr)
	}
	if got, want := file.writableCalls, 0; got != want {
		t.Fatalf("expected explicit apfs driver to avoid file fallback, got %d file calls", got)
	}
}

type stubVolumeDriver struct {
	name               string
	baseVolume         volumestore.BaseVolume
	baseErr            error
	writableVolume     volumestore.WritableVolume
	writableErr        error
	snapshot           volumestore.Snapshot
	snapshotErr        error
	clonedVolume       volumestore.WritableVolume
	cloneSnapshotErr   error
	ensureBaseCalls    int
	writableCalls      int
	snapshotCalls      int
	cloneSnapshotCalls int
}

func (d *stubVolumeDriver) Name() string { return d.name }

func (d *stubVolumeDriver) EnsureBaseVolume(context.Context, volumestore.EnsureBaseVolumeRequest) (volumestore.BaseVolume, error) {
	d.ensureBaseCalls++
	if d.baseErr != nil {
		return volumestore.BaseVolume{}, d.baseErr
	}
	return d.baseVolume, nil
}

func (d *stubVolumeDriver) CreateWritableVolume(context.Context, volumestore.CreateWritableVolumeRequest) (volumestore.WritableVolume, error) {
	d.writableCalls++
	if d.writableErr != nil {
		return volumestore.WritableVolume{}, d.writableErr
	}
	return d.writableVolume, nil
}

func (d *stubVolumeDriver) SnapshotVolume(context.Context, volumestore.SnapshotVolumeRequest) (volumestore.Snapshot, error) {
	d.snapshotCalls++
	if d.snapshotErr != nil {
		return volumestore.Snapshot{}, d.snapshotErr
	}
	return d.snapshot, nil
}

func (d *stubVolumeDriver) CloneSnapshotToVolume(context.Context, volumestore.CloneSnapshotToVolumeRequest) (volumestore.WritableVolume, error) {
	d.cloneSnapshotCalls++
	if d.cloneSnapshotErr != nil {
		return volumestore.WritableVolume{}, d.cloneSnapshotErr
	}
	return d.clonedVolume, nil
}

func (*stubVolumeDriver) DestroyVolume(context.Context, volumestore.DestroyVolumeRequest) error {
	return nil
}

func (*stubVolumeDriver) DestroySnapshot(context.Context, volumestore.DestroySnapshotRequest) error {
	return nil
}
